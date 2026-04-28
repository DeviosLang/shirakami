package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/DeviosLang/shirakami/internal/agent"
	"github.com/DeviosLang/shirakami/internal/cache"
	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/config"
	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/logger"
	"github.com/DeviosLang/shirakami/internal/report"
	"github.com/DeviosLang/shirakami/internal/storage"
	itool "github.com/DeviosLang/shirakami/internal/tool"
	"github.com/DeviosLang/shirakami/internal/workspace"
	"github.com/DeviosLang/shirakami/pkg/schema"
)

var (
	version = "0.1.0"
	cfgFile string
)

func main() {
	root := buildRoot()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func buildRoot() *cobra.Command {
	root := &cobra.Command{
		Use:     "shirakami",
		Short:   "Shirakami static analysis agent",
		Version: version,
	}
	root.PersistentFlags().StringVar(&cfgFile, "config", "", "shirakami.yaml config file path")

	root.AddCommand(
		buildAnalyzeCmd(),
		buildResultsCmd(),
		buildFeedbackCmd(),
		buildWorkspaceCmd(),
	)
	return root
}

// ---------------------------------------------------------------------------
// toolAdapter bridges internal/tool.Tool → agent.Tool
// ---------------------------------------------------------------------------

type toolAdapter struct {
	inner itool.Tool
}

func (a *toolAdapter) Definition() llm.ToolDefinition {
	schema, _ := json.Marshal(a.inner.InputSchema())
	return llm.ToolDefinition{
		Name:        a.inner.Name(),
		Description: a.inner.Description(),
		Parameters:  schema,
	}
}

func (a *toolAdapter) Execute(ctx context.Context, arguments []byte) (string, error) {
	return a.inner.Execute(ctx, json.RawMessage(arguments))
}

func defaultTools(workspaceDir string) []agent.Tool {
	tools := []itool.Tool{
		itool.NewRipgrepTool(workspaceDir),
		itool.NewGlobTool(workspaceDir),
		itool.NewReaderTool(),
		itool.GlobalLSPManager.GetOrCreate(workspaceDir),
	}
	adapted := make([]agent.Tool, len(tools))
	for i, t := range tools {
		adapted[i] = &toolAdapter{inner: t}
	}
	return adapted
}

// ---------------------------------------------------------------------------
// shirakami analyze
// ---------------------------------------------------------------------------

func buildAnalyzeCmd() *cobra.Command {
	var (
		workspaceDir string
		diffFiles    []string
		description  string
		outputFmt    string
		maxSteps     int
	)

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze code changes and output call chains",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			log := logger.Must("production")
			defer log.Sync() //nolint:errcheck

			// Ensure all gopls processes are cleaned up at session end.
			defer itool.GlobalLSPManager.Close()

			// Override workspace dir if provided.
			if workspaceDir != "" {
				cfg.Workspace.Dir = workspaceDir
			}

			// Build analysis input.
			var diffContent strings.Builder
			for _, f := range diffFiles {
				data, err := os.ReadFile(f)
				if err != nil {
					return fmt.Errorf("read diff file %s: %w", f, err)
				}
				diffContent.Write(data)
				diffContent.WriteString("\n")
			}

			input := agent.AnalysisInput{
				Diff:        diffContent.String(),
				Description: description,
			}

			// Determine input type for storage.
			inputType := storage.InputTypeCombined
			if input.Diff == "" {
				inputType = storage.InputTypeDescription
			} else if input.Description == "" {
				inputType = storage.InputTypeDiff
			}

			ctx := context.Background()

			// Set up Redis / cache.
			rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
			analysisCache := cache.New(rdb)

			// Compute cache key.
			cacheKey := cache.CacheKey(input.Diff+input.Description, []string{cfg.Workspace.Dir})

			// Check cache.
			if cached, ok := analysisCache.Get(ctx, cacheKey); ok {
				log.Sugar().Infow("cache hit", "task_id", cached.TaskID)
				return renderCachedResult(cached, report.OutputFormat(outputFmt))
			}

			// Set up DB.
			var store *storage.Store
			if cfg.DB.DSN != "" {
				pool, err := pgxpool.New(ctx, cfg.DB.DSN)
				if err != nil {
					log.Sugar().Warnw("db connect failed, skipping persistence", "err", err)
				} else {
					defer pool.Close()
					store = storage.New(pool)
				}
			}

			// Create task record.
			var taskID string
			if store != nil {
				task, err := store.CreateTask(ctx, inputType, input.Diff, input.Description, cacheKey)
				if err != nil {
					log.Sugar().Warnw("create task record failed", "err", err)
				} else {
					taskID = task.ID
					_ = store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusRunning)
				}
			}

			// Build LLM client.
			llmClient := llm.NewClient(llm.Config{
				BaseURL: cfg.LLM.Endpoint,
				APIKey:  cfg.LLM.APIKey,
				Model:   cfg.LLM.Model,
			})

			// Build tools.
			tools := defaultTools(cfg.Workspace.Dir)

			// Build repos from config.
			repos := configRepos(cfg)

			// Build checkpointer.
			cpDir := os.TempDir() + "/shirakami-checkpoints"
			cp, err := checkpoint.NewFileCheckpointer(cpDir)
			if err != nil {
				return fmt.Errorf("create checkpointer: %w", err)
			}

			// Build and run orchestrator.
			orch := agent.NewOrchestrator(llmClient, tools, repos, cfg.Workspace.Dir, cp)
			_ = maxSteps // max-steps is passed to orchestrator via constructor in future

			output, err := orch.Run(ctx, input)
			if err != nil {
				if store != nil && taskID != "" {
					_ = store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusFailed)
				}
				return fmt.Errorf("analysis failed: %w", err)
			}

			// Persist result.
			if store != nil && taskID != "" {
				callChainJSON, _ := json.Marshal(output.CallGraph)
				entryPointsJSON, _ := json.Marshal(output.EntryPoints)
				_ = store.SaveResult(ctx, &storage.TaskResult{
					TaskID:      taskID,
					CallChain:   callChainJSON,
					EntryPoints: entryPointsJSON,
				})
				_ = store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusCompleted)
			}

			// Build schema result for rendering.
			result := buildSchemaResult(taskID, output, input)

			// Cache the result.
			cacheResult := &cache.AnalysisResult{
				TaskID:    taskID,
				CreatedAt: time.Now(),
			}
			if jb, err := json.Marshal(result); err == nil {
				cacheResult.CallChain = jb
			}
			_ = analysisCache.Set(ctx, cacheKey, cacheResult, 0)

			// Render output.
			rendered, err := report.Generate(result, report.OutputFormat(outputFmt))
			if err != nil {
				return fmt.Errorf("render output: %w", err)
			}
			fmt.Print(rendered)
			return nil
		},
	}

	cmd.Flags().StringVar(&workspaceDir, "workspace", "", "local code directory")
	cmd.Flags().StringArrayVar(&diffFiles, "diff", nil, "diff file path (can specify multiple times)")
	cmd.Flags().StringVar(&description, "desc", "", "text description of the change")
	cmd.Flags().StringVar(&outputFmt, "output", "terminal", "output format: terminal / json / markdown")
	cmd.Flags().IntVar(&maxSteps, "max-steps", 100, "agent max steps")

	return cmd
}

// ---------------------------------------------------------------------------
// shirakami results
// ---------------------------------------------------------------------------

func buildResultsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "results",
		Short: "Manage analysis results",
	}
	cmd.AddCommand(buildResultsListCmd(), buildResultsGetCmd())
	return cmd
}

func buildResultsListCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent analysis tasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}
			ctx := context.Background()
			pool, err := pgxpool.New(ctx, cfg.DB.DSN)
			if err != nil {
				return fmt.Errorf("connect db: %w", err)
			}
			defer pool.Close()
			store := storage.New(pool)

			tasks, err := store.ListTasks(ctx, limit)
			if err != nil {
				return fmt.Errorf("list tasks: %w", err)
			}

			if len(tasks) == 0 {
				fmt.Println("No analysis tasks found.")
				return nil
			}

			fmt.Printf("%-36s  %-12s  %-12s  %s\n", "ID", "STATUS", "INPUT TYPE", "CREATED AT")
			fmt.Println(strings.Repeat("-", 85))
			for _, t := range tasks {
				fmt.Printf("%-36s  %-12s  %-12s  %s\n",
					t.ID, t.Status, t.InputType,
					t.CreatedAt.Local().Format(time.RFC3339),
				)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum number of results to list")
	return cmd
}

func buildResultsGetCmd() *cobra.Command {
	var id string

	cmd := &cobra.Command{
		Use:   "get",
		Short: "Get details of an analysis task",
		RunE: func(cmd *cobra.Command, args []string) error {
			if id == "" {
				return fmt.Errorf("--id is required")
			}
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}
			ctx := context.Background()
			pool, err := pgxpool.New(ctx, cfg.DB.DSN)
			if err != nil {
				return fmt.Errorf("connect db: %w", err)
			}
			defer pool.Close()
			store := storage.New(pool)

			task, err := store.GetTask(ctx, id)
			if err != nil {
				return fmt.Errorf("get task: %w", err)
			}

			fmt.Printf("ID:         %s\n", task.ID)
			fmt.Printf("Status:     %s\n", task.Status)
			fmt.Printf("InputType:  %s\n", task.InputType)
			fmt.Printf("CreatedAt:  %s\n", task.CreatedAt.Local().Format(time.RFC3339))
			if task.CompletedAt != nil {
				fmt.Printf("CompletedAt: %s\n", task.CompletedAt.Local().Format(time.RFC3339))
			}

			result, err := store.GetResult(ctx, id)
			if err == nil {
				fmt.Printf("\nTokenUsage: %d\n", result.TokenUsage)
				fmt.Printf("StepCount:  %d\n", result.StepCount)
				if len(result.CallChain) > 0 {
					fmt.Printf("CallChain:  %s\n", string(result.CallChain))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "task ID")
	return cmd
}

// ---------------------------------------------------------------------------
// shirakami feedback
// ---------------------------------------------------------------------------

func buildFeedbackCmd() *cobra.Command {
	var (
		taskID    string
		fbType    string
		fbComment string
	)

	cmd := &cobra.Command{
		Use:   "feedback",
		Short: "Submit feedback for an analysis result",
		RunE: func(cmd *cobra.Command, args []string) error {
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
			}
			validTypes := map[string]bool{
				"false_positive": true,
				"false_negative": true,
				"correct":        true,
			}
			if !validTypes[fbType] {
				return fmt.Errorf("--type must be one of: false_positive, false_negative, correct")
			}

			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}
			ctx := context.Background()
			pool, err := pgxpool.New(ctx, cfg.DB.DSN)
			if err != nil {
				return fmt.Errorf("connect db: %w", err)
			}
			defer pool.Close()

			_, err = pool.Exec(ctx,
				`INSERT INTO feedback (task_id, type, comment) VALUES ($1, $2, $3)`,
				taskID, fbType, fbComment,
			)
			if err != nil {
				return fmt.Errorf("submit feedback: %w", err)
			}

			fmt.Printf("Feedback submitted for task %s (type: %s)\n", taskID, fbType)
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "task ID to submit feedback for")
	cmd.Flags().StringVar(&fbType, "type", "", "feedback type: false_positive / false_negative / correct")
	cmd.Flags().StringVar(&fbComment, "comment", "", "optional comment")
	return cmd
}

// ---------------------------------------------------------------------------
// shirakami workspace
// ---------------------------------------------------------------------------

func buildWorkspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Manage workspace repositories",
	}
	cmd.AddCommand(buildWorkspaceSyncCmd())
	return cmd
}

func buildWorkspaceSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync all repositories in the workspace (git pull/clone)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}
			log := logger.Must("production")
			defer log.Sync() //nolint:errcheck

			repos := configToWorkspaceRepos(cfg)
			if len(repos) == 0 {
				fmt.Println("No repositories configured for sync.")
				return nil
			}

			ctx := context.Background()
			results := workspace.SyncAll(ctx, cfg.Workspace.Dir, repos)
			for name, res := range results {
				if res.Err != nil {
					fmt.Fprintf(os.Stderr, "ERROR  %s: %v\n", name, res.Err)
				} else {
					fmt.Printf("OK     %s  %s\n", name, res.CommitHash)
				}
			}
			log.Sugar().Infow("sync complete", "workspace", cfg.Workspace.Dir)
			return nil
		},
	}
	return cmd
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func configRepos(cfg *config.Config) []agent.RepoInfo {
	if cfg.Workspace.Dir == "" {
		return nil
	}
	return []agent.RepoInfo{
		{
			Name: "workspace",
			Path: cfg.Workspace.Dir,
			Role: "entry",
		},
	}
}

func configToWorkspaceRepos(_ *config.Config) []workspace.RepoConfig {
	return nil
}

func buildSchemaResult(taskID string, output *agent.AnalysisOutput, input agent.AnalysisInput) *schema.AnalysisResult {
	nodes := make([]schema.CallNode, 0, len(output.CallGraph))
	for _, n := range output.CallGraph {
		nodes = append(nodes, schema.CallNode{
			FuncName: n.Function,
			FilePath: n.File,
			Line:     n.Line,
			Repo:     n.Repo,
			NodeType: schema.NodeTypeMiddle,
		})
	}

	entryNodes := make([]schema.EntryPoint, 0, len(output.EntryPoints))
	for _, e := range output.EntryPoints {
		entryNodes = append(entryNodes, schema.EntryPoint{
			Node: schema.CallNode{
				FuncName: e.Function,
				FilePath: e.File,
				Line:     e.Line,
				Repo:     e.Repo,
				NodeType: schema.NodeTypeEntry,
			},
			Protocol: schema.ProtocolHTTP,
		})
	}

	inputType := schema.InputTypeDiff
	if input.Diff == "" {
		inputType = schema.InputTypeFuncName
	}

	return &schema.AnalysisResult{
		TaskID:    taskID,
		InputType: inputType,
		DownwardChain: schema.CallChain{
			Nodes:     nodes,
			Direction: schema.DirectionDownward,
		},
		EntryPoints: entryNodes,
		ImpactSummary: schema.ImpactSummary{
			DirectCount: len(output.ChangedFunctions),
		},
	}
}

func renderCachedResult(cached *cache.AnalysisResult, format report.OutputFormat) error {
	if len(cached.CallChain) > 0 {
		var result schema.AnalysisResult
		if err := json.Unmarshal(cached.CallChain, &result); err == nil {
			rendered, err := report.Generate(&result, format)
			if err == nil {
				fmt.Print(rendered)
				return nil
			}
		}
	}

	fmt.Printf("Cache hit for task %s (created at %s)\n",
		cached.TaskID, cached.CreatedAt.Format(time.RFC3339))
	if len(cached.CallChain) > 0 {
		fmt.Println(string(cached.CallChain))
	}
	return nil
}
