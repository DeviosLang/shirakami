package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/DeviosLang/shirakami/internal/agent"
	"github.com/DeviosLang/shirakami/internal/cache"
	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/config"
	"github.com/DeviosLang/shirakami/internal/feedback"
	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/logger"
	"github.com/DeviosLang/shirakami/internal/storage"
	itool "github.com/DeviosLang/shirakami/internal/tool"
)

var (
	version = "0.1.0"
	cfgFile string
	addr    string
)

func main() {
	root := &cobra.Command{
		Use:     "shirakami-server",
		Short:   "Shirakami HTTP API server",
		Version: version,
		RunE:    runServer,
	}

	root.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: shirakami.yaml)")
	root.Flags().StringVar(&addr, "addr", ":8080", "HTTP listen address")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runServer(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	log := logger.Must("production")
	defer log.Sync() //nolint:errcheck

	log.Sugar().Infow("starting server", "addr", addr, "workspace", cfg.Workspace.Dir)

	ctx := context.Background()

	// Connect to DB.
	pool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	// Run migrations via database/sql + goose.
	if cfg.DB.DSN != "" {
		stdDB, err := sql.Open("pgx", cfg.DB.DSN)
		if err == nil {
			if migrErr := goose.SetDialect("postgres"); migrErr == nil {
				if upErr := goose.Up(stdDB, "migrations"); upErr != nil {
					log.Sugar().Warnw("goose up failed", "err", upErr)
				}
			}
			_ = stdDB.Close()
		}
	}

	store := storage.New(pool)

	// Connect to Redis.
	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	analysisCache := cache.New(rdb)

	// Build server.
	srv := &apiServer{
		cfg:   cfg,
		store: store,
		pool:  pool,
		cache: analysisCache,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.Handle("/metrics", feedback.Handler())
	mux.HandleFunc("/api/v1/tasks", srv.handleTasks)
	mux.HandleFunc("/api/v1/tasks/", srv.handleTaskByID)

	log.Sugar().Infof("listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}

// ---------------------------------------------------------------------------
// API server
// ---------------------------------------------------------------------------

type apiServer struct {
	cfg   *config.Config
	store *storage.Store
	pool  *pgxpool.Pool
	cache *cache.Cache
}

// SubmitTaskRequest is the JSON body for POST /api/v1/tasks.
type SubmitTaskRequest struct {
	InputType string `json:"input_type"` // "diff" | "description" | "combined"
	InputDiff string `json:"input_diff"`
	InputDesc string `json:"input_desc"`
}

// TaskResponse is returned by task endpoints.
type TaskResponse struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	InputType   string     `json:"input_type"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CallChain   any        `json:"call_chain,omitempty"`
	EntryPoints any        `json:"entry_points,omitempty"`
	TokenUsage  int        `json:"token_usage,omitempty"`
	StepCount   int        `json:"step_count,omitempty"`
}

// FeedbackRequest is the JSON body for PUT /api/v1/tasks/:id/feedback.
type FeedbackRequest struct {
	Type    string `json:"type"`
	Comment string `json:"comment"`
}

func (s *apiServer) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.submitTask(w, r)
	case http.MethodGet:
		s.listTasks(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *apiServer) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	// Parse path: /api/v1/tasks/{id} or /api/v1/tasks/{id}/feedback
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
	parts := strings.SplitN(path, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch {
	case sub == "feedback" && r.Method == http.MethodPut:
		s.submitFeedback(w, r, id)
	case sub == "" && r.Method == http.MethodGet:
		s.getTask(w, r, id)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *apiServer) submitTask(w http.ResponseWriter, r *http.Request) {
	var req SubmitTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	inputType := storage.InputType(req.InputType)
	if inputType == "" {
		if req.InputDiff != "" && req.InputDesc != "" {
			inputType = storage.InputTypeCombined
		} else if req.InputDiff != "" {
			inputType = storage.InputTypeDiff
		} else {
			inputType = storage.InputTypeDescription
		}
	}

	cacheKey := cache.CacheKey(req.InputDiff+req.InputDesc, []string{s.cfg.Workspace.Dir})

	ctx := r.Context()
	task, err := s.store.CreateTask(ctx, inputType, req.InputDiff, req.InputDesc, cacheKey)
	if err != nil {
		jsonError(w, "failed to create task: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Launch analysis in background.
	go s.runAnalysis(task.ID, req.InputDiff, req.InputDesc, cacheKey)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(TaskResponse{ //nolint:errcheck
		ID:        task.ID,
		Status:    string(task.Status),
		InputType: string(task.InputType),
		CreatedAt: task.CreatedAt,
	})
}

func (s *apiServer) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ListTasks(r.Context(), 20)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	responses := make([]TaskResponse, 0, len(tasks))
	for _, t := range tasks {
		responses = append(responses, TaskResponse{
			ID:          t.ID,
			Status:      string(t.Status),
			InputType:   string(t.InputType),
			CreatedAt:   t.CreatedAt,
			CompletedAt: t.CompletedAt,
		})
	}
	jsonOK(w, responses)
}

func (s *apiServer) getTask(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	task, err := s.store.GetTask(ctx, id)
	if err != nil {
		if err == storage.ErrNotFound {
			jsonError(w, "task not found", http.StatusNotFound)
		} else {
			jsonError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	resp := TaskResponse{
		ID:          task.ID,
		Status:      string(task.Status),
		InputType:   string(task.InputType),
		CreatedAt:   task.CreatedAt,
		CompletedAt: task.CompletedAt,
	}

	if task.Status == storage.TaskStatusCompleted {
		result, err := s.store.GetResult(ctx, id)
		if err == nil {
			resp.TokenUsage = result.TokenUsage
			resp.StepCount = result.StepCount
			var callChain any
			if err := json.Unmarshal(result.CallChain, &callChain); err == nil {
				resp.CallChain = callChain
			}
			var entryPoints any
			if err := json.Unmarshal(result.EntryPoints, &entryPoints); err == nil {
				resp.EntryPoints = entryPoints
			}
		}
	}

	jsonOK(w, resp)
}

func (s *apiServer) submitFeedback(w http.ResponseWriter, r *http.Request, taskID string) {
	var req FeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	valid := map[string]bool{
		"false_positive": true,
		"false_negative": true,
		"correct":        true,
	}
	if !valid[req.Type] {
		jsonError(w, "type must be one of: false_positive, false_negative, correct", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	// Verify task exists.
	if _, err := s.store.GetTask(ctx, taskID); err != nil {
		if err == storage.ErrNotFound {
			jsonError(w, "task not found", http.StatusNotFound)
		} else {
			jsonError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO feedback (task_id, type, comment) VALUES ($1, $2, $3)`,
		taskID, req.Type, req.Comment,
	)
	if err != nil {
		jsonError(w, "failed to submit feedback: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"status": "ok"})
}

// runAnalysis runs the analysis in a goroutine and saves the result.
func (s *apiServer) runAnalysis(taskID, inputDiff, inputDesc, cacheKey string) {
	ctx := context.Background()

	// Check cache first.
	if cached, ok := s.cache.Get(ctx, cacheKey); ok {
		_ = s.store.SaveResult(ctx, &storage.TaskResult{
			TaskID:      taskID,
			CallChain:   cached.CallChain,
			EntryPoints: cached.EntryPoints,
		})
		_ = s.store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusCompleted)
		return
	}

	_ = s.store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusRunning)

	llmClient := llm.NewClient(llm.Config{
		BaseURL: s.cfg.LLM.Endpoint,
		APIKey:  s.cfg.LLM.APIKey,
		Model:   s.cfg.LLM.Model,
	})

	tools := defaultTools(s.cfg.Workspace.Dir)
	repos := configRepos(s.cfg)

	cpDir := os.TempDir() + "/shirakami-checkpoints"
	cp, err := checkpoint.NewFileCheckpointer(cpDir)
	if err != nil {
		_ = s.store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusFailed)
		return
	}

	orch := agent.NewOrchestrator(llmClient, tools, repos, s.cfg.Workspace.Dir, cp)
	output, err := orch.Run(ctx, agent.AnalysisInput{
		Diff:        inputDiff,
		Description: inputDesc,
	})
	if err != nil {
		_ = s.store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusFailed)
		return
	}

	callChainJSON, _ := json.Marshal(output.CallGraph)
	entryPointsJSON, _ := json.Marshal(output.EntryPoints)
	_ = s.store.SaveResult(ctx, &storage.TaskResult{
		TaskID:      taskID,
		CallChain:   callChainJSON,
		EntryPoints: entryPointsJSON,
	})
	_ = s.store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusCompleted)

	cacheResult := &cache.AnalysisResult{
		TaskID:      taskID,
		CallChain:   callChainJSON,
		EntryPoints: entryPointsJSON,
		CreatedAt:   time.Now(),
	}
	_ = s.cache.Set(ctx, cacheKey, cacheResult, 0)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func defaultTools(workspaceDir string) []agent.Tool {
	tools := []itool.Tool{
		itool.NewRipgrepTool(workspaceDir),
		itool.NewGlobTool(workspaceDir),
		itool.NewReaderTool(),
	}
	adapted := make([]agent.Tool, len(tools))
	for i, t := range tools {
		adapted[i] = &toolAdapter{inner: t}
	}
	return adapted
}

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

type toolAdapter struct {
	inner itool.Tool
}

func (a *toolAdapter) Definition() llm.ToolDefinition {
	s, _ := json.Marshal(a.inner.InputSchema())
	return llm.ToolDefinition{
		Name:        a.inner.Name(),
		Description: a.inner.Description(),
		Parameters:  s,
	}
}

func (a *toolAdapter) Execute(ctx context.Context, arguments []byte) (string, error) {
	return a.inner.Execute(ctx, json.RawMessage(arguments))
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}
