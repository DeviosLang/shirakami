package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/DeviosLang/shirakami/internal/agent"
	"github.com/DeviosLang/shirakami/internal/benchmark"
	"github.com/DeviosLang/shirakami/internal/cache"
	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/config"
	"github.com/DeviosLang/shirakami/internal/index"
	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/logger"
	"github.com/DeviosLang/shirakami/internal/report"
	"github.com/DeviosLang/shirakami/internal/resolve"
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
		buildIndexCmd(),
		buildBenchmarkCmd(),
		buildContractCmd(),
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

// multiRepoTools returns a single tool set rooted at the workspace directory.
// ripgrep and glob both accept an optional "repo" parameter so the LLM can
// dynamically restrict searches to any repository without requiring per-repo
// tool instances.  New repos need only be listed in shirakami.yaml — no code
// change or image rebuild required.
func multiRepoTools(workspaceDir string) []agent.Tool {
	tools := []itool.Tool{
		itool.NewRipgrepTool(workspaceDir), // supports repo= parameter
		itool.NewGlobTool(workspaceDir),    // supports repo= parameter
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
		workspaceDir   string
		diffFiles      []string
		description    string
		outputFmt      string
		maxSteps       int
		sourceRepo     string
		noCache        bool
		analysisConfig string
		fastMode       bool
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
			// Install as default logger for agent / worker / triage packages.
			logger.SetDefault(log)

			// Ensure all gopls processes are cleaned up at session end.
			defer itool.GlobalLSPManager.Close()

			// Override workspace dir if provided.
			if workspaceDir != "" {
				cfg.Workspace.Dir = workspaceDir
			}

			// Build analysis input.
			// Priority: --analysis (YAML) > --diff (one or more diff files)
			var input agent.AnalysisInput
			if analysisConfig != "" {
				// YAML config mode — supports multiple patches + scope filter.
				acfg, err := agent.LoadAnalysisConfig(analysisConfig)
				if err != nil {
					return err
				}
				input, err = acfg.ToAnalysisInput()
				if err != nil {
					return err
				}
				// --desc / --source-repo override YAML values if explicitly provided.
				if description != "" {
					input.Description = description
				}
				if sourceRepo != "" {
					input.SourceRepo = sourceRepo
				}
				log.Sugar().Infow("analysis.config_loaded",
					"path", analysisConfig,
					"patches", len(input.PatchInfo),
					"source_repo", input.SourceRepo,
					"scope_only_repos", input.ScopeOnlyRepos,
				)
			} else {
				// Legacy mode — one or more --diff files.
				var diffContent strings.Builder
				for _, f := range diffFiles {
					data, err := os.ReadFile(f)
					if err != nil {
						return fmt.Errorf("read diff file %s: %w", f, err)
					}
					diffContent.Write(data)
					diffContent.WriteString("\n")
				}
				input = agent.AnalysisInput{
					Diff:        diffContent.String(),
					Description: description,
					SourceRepo:  sourceRepo,
				}
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
			rdb := redis.NewClient(&redis.Options{
				Addr:     cfg.Redis.Addr,
				Password: cfg.Redis.Password,
			})
			analysisCache := cache.New(rdb)

			// Compute cache key.
			cacheKey := cache.CacheKey(input.Diff+input.Description+input.SourceRepo, []string{cfg.Workspace.Dir})

			// Check cache (skip if --no-cache).
			if !noCache {
				if cached, ok := analysisCache.Get(ctx, cacheKey); ok {
					log.Sugar().Infow("cache hit", "task_id", cached.TaskID)
					return renderCachedResult(cached, report.OutputFormat(outputFmt))
				}
			} else {
				log.Sugar().Infow("cache skipped (--no-cache)")
			}

			// Set up DB.
			var store *storage.Store
			var pool *pgxpool.Pool
			if cfg.DB.DSN != "" {
				var err error
				pool, err = pgxpool.New(ctx, cfg.DB.DSN)
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
				task, err := store.CreateTask(ctx, inputType, input.Diff, input.Description, cacheKey, sourceRepo, nil)
				if err != nil {
					log.Sugar().Warnw("create task record failed", "err", err)
				} else {
					taskID = task.ID
					_ = store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusRunning)
				}
			}

			// Build LLM client.
			llmClient := llm.NewClient(llm.Config{
				BaseURL:        cfg.LLM.Endpoint,
				APIKey:         cfg.LLM.APIKey,
				Model:          cfg.LLM.Model,
				RequestTimeout: cfg.LLM.RequestTimeout,
			})

			// Build repos from config.
			repos := configRepos(cfg)

			// Mark the source repo so LSP + tools are prioritised for it.
			if sourceRepo != "" {
				for i := range repos {
					if repos[i].Name == sourceRepo && repos[i].Role == "" {
						repos[i].Role = "source"
					}
				}
			}

			// Build tools: workspace-rooted tools with dynamic repo parameter.
			var tools []agent.Tool
			if len(repos) > 1 {
				tools = multiRepoTools(cfg.Workspace.Dir)
			} else {
				tools = defaultTools(cfg.Workspace.Dir)
			}

			// Build checkpointer.
			cpDir := os.TempDir() + "/shirakami-checkpoints"
			cp, err := checkpoint.NewFileCheckpointer(cpDir)
			if err != nil {
				return fmt.Errorf("create checkpointer: %w", err)
			}

			// Build and run orchestrator.
			orch := agent.NewOrchestrator(llmClient, tools, repos, cfg.Workspace.Dir, cp)
			if fastMode {
				// Fast mode: cap cross-repo iterations at 3 rounds (default is 10).
				// Trades coverage for speed — typically 2-3x faster, keeps direct
				// and one-hop upstream coverage, skips deep transitive exploration.
				orch.SetMaxRounds(3)
			} else if cfg.MaxRounds > 0 {
				// Apply max_rounds from config when --fast flag is not set.
				orch.SetMaxRounds(cfg.MaxRounds)
			}
			// Apply P1 step budget from config (default 150 via config default).
			if cfg.P1StepBudget > 0 {
				orch.SetP1StepBudget(cfg.P1StepBudget)
			}
			// Apply P0 step budget from config (default 0 = no cap).
			if cfg.P0StepBudget > 0 {
				orch.SetP0StepBudget(cfg.P0StepBudget)
			}

			// Inject declared contract hints from config into Worker prompts.
			if len(cfg.Contracts) > 0 {
				hints := FormatContractHints(cfg.Contracts)
				orch.SetContractHints(hints)
				log.Sugar().Infow("contracts.loaded", "count", len(hints))
			}

			// Index mode: load symbol graph for hybrid/deterministic analysis.
			// Priority: --index-mode CLI flag > config.index_mode > "off".
			indexMode := cfg.IndexMode
			if indexMode == "" {
				indexMode = "off"
			}
			if cmd.Flags().Changed("index-mode") {
				indexMode, _ = cmd.Flags().GetString("index-mode")
			}
			orch.SetIndexMode(indexMode)

			// Provide the DB pool so extractChangedFunctions can use Layer B
			// (DiffToSymbols) when index mode is active, without requiring the
			// full in-memory graph to be loaded first.
			if pool != nil {
				orch.SetPool(pool)
			}

			if indexMode != "off" && pool != nil {
				// Try to load index graph from DB
				idxStore := index.NewStore(pool)
				repoNames := make([]string, 0, len(repos))
				for _, r := range repos {
					repoNames = append(repoNames, r.Name)
				}
				nodes, _ := idxStore.LoadAllNodes(ctx, repoNames)
				edges, _ := idxStore.LoadAllEdges(ctx, repoNames)
				if len(nodes) > 0 {
					graph := index.NewInMemoryGraph()
					graph.Load(nodes, edges)
					orch.SetIndexGraph(&graphAdapter{graph: graph})
					// Wire the resolve.Resolver so runGraphAnalysis uses the richer
					// path: symbol disambiguation, risk assessment, entry-point
					// detection, and cross-repo hop tracking.
					orch.SetResolver(resolve.New(graph))
					log.Sugar().Infow("index.graph_loaded",
						"nodes", graph.NodeCount(),
						"edges", graph.EdgeCount(),
						"mode", indexMode,
					)

					// Build import context for Python repos (reduces LLM search rounds)
					changedFiles := extractDiffFilesFromInput(input)
					importCtx := index.BuildImportContext(nodes, edges, input.SourceRepo, changedFiles)
					if importCtx != "" {
						orch.SetImportContext(importCtx)
						log.Sugar().Infow("index.import_context_built",
							"repo", input.SourceRepo,
							"context_bytes", len(importCtx),
						)
					}
				} else {
					log.Sugar().Infow("index.graph_empty", "mode", indexMode,
						"hint", "run 'shirakami index update' to build the symbol index")
				}
			}

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
				// Build structured storage from parsed WorkerResult data.
				// output.EntryPoints is populated by parseWorkerOutput from JSON.
				// output.CallGraph contains parsed CallNodes (not raw text) when JSON parsing succeeded.
				// Filter out fallback nodes where Function contains raw LLM text (>500 chars).
				cleanNodes := make([]agent.CallNode, 0, len(output.CallGraph))
				for _, n := range output.CallGraph {
					if len(n.Function) < 500 && n.File != "" {
						cleanNodes = append(cleanNodes, n)
					}
				}

				callChainJSON, _ := json.Marshal(cleanNodes)
				entryPointsJSON, _ := json.Marshal(output.EntryPoints)

				// Also extract structured data from WorkerOutputs for richer storage.
				type storageEntry struct {
					Repo     string `json:"repo"`
					File     string `json:"file"`
					Line     int    `json:"line"`
					Function string `json:"function"`
					Endpoint string `json:"endpoint,omitempty"`
				}
				var structuredEntries []storageEntry
				for _, ep := range output.EntryPoints {
					if ep.Repo != "" && ep.Function != "" {
						structuredEntries = append(structuredEntries, storageEntry{
							Repo:     ep.Repo,
							File:     ep.File,
							Line:     ep.Line,
							Function: ep.Function,
						})
					}
				}
				if len(structuredEntries) > 0 {
					entryPointsJSON, _ = json.Marshal(structuredEntries)
				}

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

			// Print shadow parity report if available.
			if output.ShadowReport != nil && output.ShadowReport.Details != "" {
				fmt.Printf("\n%s\n", output.ShadowReport.Details)

				// Convert agent.ShadowParityReport → benchmark.ParityReport and persist.
				parityReport := shadowToParityReport(output.ShadowReport, input.SourceRepo, description)
				shadowDir := filepath.Join(workspaceDir, "reports", "shadow-parity")
				if serr := benchmark.SaveReport(parityReport, shadowDir); serr != nil {
					logger.S().Warnw("shadow.save_failed", "err", serr)
				} else {
					trendPath := filepath.Join(shadowDir, "parity-trend.csv")
					if terr := benchmark.AppendTrend(trendPath, parityReport); terr != nil {
						logger.S().Warnw("shadow.trend_failed", "err", terr)
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&workspaceDir, "workspace", "", "local code directory")
	cmd.Flags().StringArrayVar(&diffFiles, "diff", nil, "diff file path (can specify multiple times)")
	cmd.Flags().StringVar(&description, "desc", "", "text description of the change")
	cmd.Flags().StringVar(&outputFmt, "output", "terminal", "output format: terminal / json / markdown")
	cmd.Flags().IntVar(&maxSteps, "max-steps", 100, "agent max steps")
	cmd.Flags().StringVar(&sourceRepo, "source-repo", "", "repo name where the diff originates (used to route changed functions)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "skip cache lookup and force fresh analysis")
	cmd.Flags().StringVar(&analysisConfig, "analysis", "", "YAML analysis config file (supports multiple patches + scope filter); when set, overrides --diff")
	cmd.Flags().BoolVar(&fastMode, "fast", false, "fast mode: limit cross-repo rounds to 3 (default is deep mode with 10 rounds)")
	cmd.Flags().String("index-mode", "", "index usage mode: off (pure LLM), shadow, hybrid, deterministic (default from config.index_mode)")

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

func extractDiffFilesFromInput(input agent.AnalysisInput) []string {
	// Extract changed file paths from diff using ParseDiffHunks
	if input.Diff == "" {
		return nil
	}
	hunks := itool.ParseDiffHunks(input.Diff)
	seen := make(map[string]bool)
	var files []string
	for _, h := range hunks {
		if !seen[h.File] {
			seen[h.File] = true
			files = append(files, h.File)
		}
	}
	return files
}

func configRepos(cfg *config.Config) []agent.RepoInfo {
	// Multi-repo mode: use repos defined in workspace.repos config.
	if len(cfg.Workspace.Repos) > 0 {
		repos := make([]agent.RepoInfo, 0, len(cfg.Workspace.Repos))
		for _, r := range cfg.Workspace.Repos {
			repos = append(repos, agent.RepoInfo{
				Name: r.Name,
				Path: cfg.Workspace.Dir + "/" + r.Name,
				Role: r.Role,
			})
		}
		return repos
	}
	// Single-repo fallback: treat workspace.dir itself as the only repo.
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

func configToWorkspaceRepos(cfg *config.Config) []workspace.RepoConfig {
	repos := make([]workspace.RepoConfig, 0, len(cfg.Workspace.Repos))
	for _, r := range cfg.Workspace.Repos {
		repos = append(repos, workspace.RepoConfig{
			Name:   r.Name,
			URL:    r.URL,
			Branch: r.Branch,
		})
	}
	return repos
}

func buildSchemaResult(taskID string, output *agent.AnalysisOutput, input agent.AnalysisInput) *schema.AnalysisResult {
	nodes := make([]schema.CallNode, 0, len(output.CallGraph))
	seenNodes := make(map[string]bool)
	crossRepoSet := make(map[string]bool)
	for _, n := range output.CallGraph {
		// Skip internal truncation placeholders.
		if strings.Contains(n.Function, "[truncated — too many changed functions]") {
			continue
		}
		// Skip raw LLM prose fallback nodes (no file, very long function text).
		if n.File == "" && len(n.Function) > 200 {
			continue
		}
		// Deduplicate by repo+file+function.
		nodeKey := n.Repo + "|" + n.File + "|" + n.Function
		if seenNodes[nodeKey] {
			continue
		}
		seenNodes[nodeKey] = true

		role := schema.NodeTypeMiddle
		for _, ep := range output.EntryPoints {
			if ep.Repo == n.Repo && ep.Function == n.Function {
				role = schema.NodeTypeEntry
				break
			}
		}
		nodes = append(nodes, schema.CallNode{
			FuncName: n.Function,
			FilePath: n.File,
			Line:     n.Line,
			Repo:     n.Repo,
			NodeType: role,
			Source:   schema.NodeSource(n.Source),
		})
		// Collect cross-repo impact.
		if n.Repo != "" && n.Repo != input.SourceRepo {
			crossRepoSet[n.Repo] = true
		}
	}

	// Deduplicate entry points by repo+function (preserve first occurrence).
	seenEntries := make(map[string]bool)
	entryNodes := make([]schema.EntryPoint, 0, len(output.EntryPoints))
	for _, e := range output.EntryPoints {
		epKey := e.Repo + "|" + e.Function
		if seenEntries[epKey] {
			continue
		}
		seenEntries[epKey] = true
		entryNodes = append(entryNodes, schema.EntryPoint{
			Node: schema.CallNode{
				FuncName: e.Function,
				FilePath: e.File,
				Line:     e.Line,
				Repo:     e.Repo,
				NodeType: schema.NodeTypeEntry,
				Source:   schema.NodeSource(e.Source),
			},
			Protocol: schema.ProtocolHTTP,
			Source:   schema.NodeSource(e.Source),
		})
	}

	// Merge per-entry-point scenario data from WorkerResult.EntryScenarios into entryNodes.
	// Matching uses a multi-level fallback because the LLM may use slightly different names:
	//   1. Exact FuncName match
	//   2. Substring match — only when the shorter string is at least 6 chars, to avoid
	//      short common tokens like "Create" matching "CreateUser" AND "CreateInstance"
	//      (keeps the best/longest match when multiple candidates qualify)
	//   3. FilePath match (entry_file vs node FilePath)
	mergeScenario := func(es agent.EntryPointScenario) {
		idx := findEntryNodeIdx(entryNodes, es.EntryFunction, es.EntryFile)
		if idx < 0 {
			return
		}
		entryNodes[idx].ChangedVia = es.ChangedVia
		entryNodes[idx].Preconditions = es.Preconditions
		entryNodes[idx].TypicalInputs = es.TypicalInputs
		for _, s := range es.Scenarios {
			sc := schema.SuggestedTestScenario{
				Type:        s.Type,
				Description: s.Description,
				Input:       s.Input,
				Expected:    s.Expected,
				Priority:    s.Priority,
			}
			for _, o := range s.Oracles {
				sc.Oracles = append(sc.Oracles, schema.TestOracle{
					Type:      o.Type,
					Target:    o.Target,
					Assertion: o.Assertion,
				})
			}
			entryNodes[idx].SuggestedScenarios = append(entryNodes[idx].SuggestedScenarios, sc)
		}
	}
	for _, wr := range output.WorkerOutputs {
		if wr == nil {
			continue
		}
		for _, es := range wr.EntryScenarios {
			mergeScenario(es)
		}
	}

	// Convert FunctionAnalyses (constraints + test scenarios).
	funcAnalyses := make([]schema.FunctionAnalysis, 0, len(output.FunctionAnalyses))
	for _, fa := range output.FunctionAnalyses {
		sfa := schema.FunctionAnalysis{
			FuncName:      fa.Name,
			Repo:          fa.Repo,
			FilePath:      fa.File,
			ExistingTests: fa.ExistingTests,
		}
		for _, c := range fa.Constraints {
			sfa.Constraints = append(sfa.Constraints, schema.FunctionConstraint{
				Type:      c.Type,
				Condition: c.Condition,
				FilePath:  c.File,
				Line:      c.Line,
				Note:      c.Note,
			})
		}
		for _, s := range fa.SuggestedScenarios {
			sfa.SuggestedScenarios = append(sfa.SuggestedScenarios, schema.SuggestedTestScenario{
				Type:        s.Type,
				Description: s.Description,
				Input:       s.Input,
				Expected:    s.Expected,
				Priority:    s.Priority,
			})
		}
		funcAnalyses = append(funcAnalyses, sfa)
	}

	inputType := schema.InputTypeDiff
	if input.Diff == "" {
		inputType = schema.InputTypeFuncName
	}

	// Collect worker raw outputs for display.
	// Show raw content when there are no structured nodes with file paths,
	// so the user always sees the LLM's analysis even when JSON nodes are empty.
	var workerRawOutputs strings.Builder
	for repoName, wr := range output.WorkerOutputs {
		if wr == nil || wr.RawContent == "" {
			continue
		}
		hasStructuredNodes := len(wr.Nodes) > 0 && wr.Nodes[0].File != ""
		hasEntryPoints := len(wr.EntryPoints) > 0
		if !hasStructuredNodes && !hasEntryPoints {
			workerRawOutputs.WriteString(fmt.Sprintf("\n### [%s]\n\n", repoName))
			workerRawOutputs.WriteString(wr.RawContent)
			workerRawOutputs.WriteString("\n")
		}
	}

	crossRepoImpact := make([]string, 0, len(crossRepoSet))
	for repo := range crossRepoSet {
		crossRepoImpact = append(crossRepoImpact, repo)
	}
	// Also surface repos discovered via deterministic cross-repo hop tracking.
	// These may not appear in CallGraph nodes if the graph path returned before
	// LLM Workers ran (e.g. deterministic mode), but they should still appear
	// in the ImpactSummary.
	for _, hop := range output.CrossRepoHops {
		if hop.ToRepo != "" && hop.ToRepo != input.SourceRepo && !crossRepoSet[hop.ToRepo] {
			crossRepoSet[hop.ToRepo] = true
			crossRepoImpact = append(crossRepoImpact, hop.ToRepo)
		}
	}
	sort.Strings(crossRepoImpact)

	// Translate deterministic cross-repo hops to schema type.
	schemaCrossRepoHops := make([]schema.CrossRepoHop, 0, len(output.CrossRepoHops))
	for _, h := range output.CrossRepoHops {
		schemaCrossRepoHops = append(schemaCrossRepoHops, schema.CrossRepoHop{
			FromRepo: h.FromRepo,
			FromFunc: h.FromFunc,
			ToRepo:   h.ToRepo,
			ToFunc:   h.ToFunc,
			Depth:    h.Depth,
			EdgeType: h.EdgeType,
		})
	}

	// Collect UT suggestions from all Workers, dedupe by (repo, file, func).
	utSeen := make(map[string]bool)
	utSuggestions := make([]schema.UTSuggestion, 0)
	for repoName, wr := range output.WorkerOutputs {
		if wr == nil {
			continue
		}
		for _, ut := range wr.UTAnalyses {
			key := repoName + "|" + ut.FilePath + "|" + ut.FuncName
			if utSeen[key] {
				continue
			}
			utSeen[key] = true
			item := schema.UTSuggestion{
				FuncName:      ut.FuncName,
				Repo:          repoName,
				FilePath:      ut.FilePath,
				Summary:       ut.Summary,
				Constraints:   ut.Constraints,
				ExistingTests: ut.ExistingTests,
			}
			for _, s := range ut.Scenarios {
				item.Scenarios = append(item.Scenarios, schema.UTScenario{
					Priority:    s.Priority,
					Type:        s.Type,
					Description: s.Description,
					MockSetup:   s.MockSetup,
					Assertions:  s.Assertions,
				})
			}
			utSuggestions = append(utSuggestions, item)
		}
	}

	return &schema.AnalysisResult{
		TaskID:    taskID,
		InputType: inputType,
		DownwardChain: schema.CallChain{
			Nodes:     nodes,
			Direction: schema.DirectionDownward,
		},
		EntryPoints:      entryNodes,
		FunctionAnalyses: funcAnalyses,
		UTSuggestions:    utSuggestions,
		ImpactSummary: schema.ImpactSummary{
			SourceRepo:      input.SourceRepo,
			DirectFunctions: output.ChangedFunctions,
			DirectCount:     len(output.ChangedFunctions),
			CrossRepoImpact: crossRepoImpact,
			CrossRepoCount:  len(crossRepoImpact),
		},
		SelfCheckReport: workerRawOutputs.String(),
		Risk:            output.Risk,
		IndexCoverage:   output.IndexCoverage,
		CrossRepoHops:   schemaCrossRepoHops,
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

// findEntryNodeIdx returns the index in entryNodes that best matches the given
// entryFunction / entryFile pair, using a three-level fallback strategy:
//
//  1. Exact FuncName match (highest confidence, immediate return)
//  2. Substring match — requires the shorter side to be ≥6 chars to prevent
//     short tokens ("Create") from matching multiple longer names; picks the
//     longest shorter-side among all qualifying candidates
//  3. FilePath match — used only when no name match has been found
//
// Returns -1 when no match is found.
func findEntryNodeIdx(entryNodes []schema.EntryPoint, entryFunction, entryFile string) int {
	idx := -1
	bestMatchLen := 0
	for i := range entryNodes {
		fn := entryNodes[i].Node.FuncName
		fp := entryNodes[i].Node.FilePath
		// Level 1: exact name match.
		if fn == entryFunction {
			return i
		}
		// Level 2: substring match with minimum-length guard.
		if strings.Contains(fn, entryFunction) || strings.Contains(entryFunction, fn) {
			shorter := len(entryFunction)
			if len(fn) < shorter {
				shorter = len(fn)
			}
			if shorter >= 6 && shorter > bestMatchLen {
				bestMatchLen = shorter
				idx = i
			}
		}
		// Level 3: file path match — only when no name match yet.
		if idx < 0 && entryFile != "" && fp != "" &&
			(strings.Contains(fp, entryFile) || strings.Contains(entryFile, fp)) {
			idx = i
		}
	}
	return idx
}

// shadowToParityReport converts the lightweight agent.ShadowParityReport into a
// full benchmark.ParityReport suitable for SaveReport / AppendTrend.
// DiffRecord detail is not available here (that would require rerunning
// NormalizeV1Result + CompareResults), so Records is left empty — the aggregate
// counters are still populated, which is sufficient for trend tracking.
func shadowToParityReport(sr *agent.ShadowParityReport, sourceRepo, description string) *benchmark.ParityReport {
	now := time.Now()
	report := &benchmark.ParityReport{
		RunID:             now.Format("2006-01-02T15-04-05"),
		Timestamp:         now,
		SourceRepo:        sourceRepo,
		Description:       description,
		TotalEdgesV1:      sr.TotalEdgesLLM,
		TotalEdgesV2:      sr.TotalEdgesGraph,
		MatchCount:        sr.MatchCount,
		MissCount:         sr.MissCount,
		ExtraPendingCount: sr.ExtraPendingCount,
		EntryPointsV1:     sr.EntryPointsLLM,
		EntryPointsV2:     sr.EntryPointsGraph,
		EntryPointMatch:   sr.EntryPointMatch,
		EntryPointMiss:    sr.EntryPointMiss,
		MissRate:          sr.MissRate,
	}
	return report
}
