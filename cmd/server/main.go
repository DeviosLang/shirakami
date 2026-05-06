package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"os/exec"
	"path/filepath"

	"github.com/DeviosLang/shirakami/internal/agent"
	"github.com/DeviosLang/shirakami/internal/cache"
	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/config"
	"github.com/DeviosLang/shirakami/internal/feedback"
	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/logger"
	"github.com/DeviosLang/shirakami/internal/storage"
	itool "github.com/DeviosLang/shirakami/internal/tool"
	"github.com/DeviosLang/shirakami/internal/webhook"
	"github.com/DeviosLang/shirakami/internal/workspace"
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
	root.Flags().StringVar(&addr, "addr", "", "HTTP listen address (overrides config)")

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

	// CLI flag overrides config.
	listenAddr := cfg.Server.Addr
	if addr != "" {
		listenAddr = addr
	}

	log.Sugar().Infow("starting server",
		"addr", listenAddr,
		"workspace", cfg.Workspace.Dir,
		"max_concurrent", cfg.Server.MaxConcurrentAnalyses,
		"default_modes", cfg.Server.DefaultModes,
	)

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
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
	})
	analysisCache := cache.New(rdb)

	// Resolve concurrency limit: must be at least 1.
	maxConcurrent := cfg.Server.MaxConcurrentAnalyses
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	// Build server.
	srv := &apiServer{
		cfg:          cfg,
		store:        store,
		pool:         pool,
		cache:        analysisCache,
		semaphore:    make(chan struct{}, maxConcurrent),
		queueCounter: new(atomic.Int64),
	}

	// Build webhook handler.
	var commenter webhook.Commenter
	if cfg.Webhook.CommentOnMR {
		if cfg.Webhook.GitLabToken != "" {
			commenter = &webhook.GitLabCommenter{
				Token:   cfg.Webhook.GitLabToken,
				BaseURL: "https://gitlab.com",
			}
		} else if cfg.Webhook.GitHubToken != "" {
			commenter = &webhook.GitHubCommenter{
				Token: cfg.Webhook.GitHubToken,
			}
		}
	}
	webhookHandler := webhook.New(
		&storageTaskAdapter{store: store},
		webhook.Config{
			Secret:    cfg.Webhook.Secret,
			Commenter: commenter,
			Launch: func(taskID, inputDiff, inputDesc, cacheKey string) {
				go srv.runAnalysis(taskID, inputDiff, inputDesc, cacheKey, "", cfg.Server.DefaultModes)
			},
		},
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.Handle("/metrics", feedback.Handler())
	mux.HandleFunc("/api/v1/repos", srv.handleRepos)
	mux.HandleFunc("/api/v1/tasks", srv.handleTasks)
	mux.HandleFunc("/api/v1/tasks/", srv.handleTaskByID)
	mux.Handle("/api/v1/webhook", webhookHandler)

	log.Sugar().Infof("listening on %s", listenAddr)
	return http.ListenAndServe(listenAddr, mux)
}

// ---------------------------------------------------------------------------
// API server
// ---------------------------------------------------------------------------

type apiServer struct {
	cfg   *config.Config
	store *storage.Store
	pool  *pgxpool.Pool
	cache *cache.Cache

	// semaphore limits concurrent analysis jobs.
	// Capacity == cfg.Server.MaxConcurrentAnalyses (default 1).
	semaphore chan struct{}

	// queueCounter tracks the number of analyses waiting in queue.
	queueCounter *atomic.Int64

	// progressMu protects the progress map.
	progressMu sync.RWMutex
	// progress maps taskID → step count for in-flight analyses.
	progress map[string]int
}

// SubmitTaskRequest is the JSON body for POST /api/v1/tasks.
type SubmitTaskRequest struct {
	InputType  string   `json:"input_type"`  // "diff" | "description" | "combined"
	InputDiff  string   `json:"input_diff"`
	InputDesc  string   `json:"input_desc"`
	SourceRepo string   `json:"source_repo"` // optional: source repo name (matches workspace.repos[].name)
	// InputBranch — when set together with SourceRepo, the server will:
	//   1. git fetch the repo on NFS
	//   2. compute git diff <base_branch>...<input_branch> automatically
	// input_diff is then ignored if input_branch resolves successfully.
	// base_branch defaults to "master" (the repo's configured branch).
	InputBranch string   `json:"input_branch,omitempty"` // feature/fix branch to diff against base

	// Branches supports multi-repo / multi-branch analysis in one request.
	// Each entry specifies a repo + branch to diff. The server fetches all diffs
	// in parallel and concatenates them for the orchestrator.
	// Takes precedence over input_branch / source_repo when non-empty.
	//
	// Example:
	//   {"branches": [
	//     {"repo": "vstation_compute", "branch": "feature/fix-margin"},
	//     {"repo": "vstation_api",     "branch": "feature/fix-margin"}
	//   ]}
	Branches []BranchEntry `json:"branches,omitempty"`

	Modes []string `json:"modes"` // optional: ["chain","e2e","ut"] – empty = all
}

// BranchEntry is one repo+branch pair inside the Branches list.
type BranchEntry struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

// RepoInfo is returned by GET /api/v1/repos so callers know valid repo names.
type RepoInfoResponse struct {
	Name       string `json:"name"`
	Branch     string `json:"branch"`      // configured base branch
	Role       string `json:"role"`
	URL        string `json:"url"`
	LocalPath  string `json:"local_path"`  // absolute path on NFS (informational)
}

// TaskResponse is returned by task endpoints.
type TaskResponse struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	InputType   string     `json:"input_type"`
	SourceRepo  string     `json:"source_repo,omitempty"`
	Modes       []string   `json:"modes,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// Queue / progress info (pending/running states).
	QueuePosition *int `json:"queue_position,omitempty"`
	Progress      int  `json:"progress,omitempty"` // step count while running

	// Completed result fields.
	CallChain        any    `json:"call_chain,omitempty"`
	EntryPoints      any    `json:"entry_points,omitempty"`
	TokenUsage       int    `json:"token_usage,omitempty"`
	StepCount        int    `json:"step_count,omitempty"`
	UTSuggestions    string `json:"ut_suggestions,omitempty"`
	FunctionAnalyses any    `json:"function_analyses,omitempty"`
	ImpactSummary    string `json:"impact_summary,omitempty"`
	CrossRepoHops    int    `json:"cross_repo_hops,omitempty"`
	Risk             string `json:"risk,omitempty"`
	IndexCoverage    any    `json:"index_coverage,omitempty"`
}

// FeedbackRequest is the JSON body for PUT /api/v1/tasks/:id/feedback.
type FeedbackRequest struct {
	Type    string `json:"type"`
	Comment string `json:"comment"`
}

// handleRepos returns the list of configured repos so callers know valid
// source_repo / branches[].repo names and their configured base branches.
//
// GET /api/v1/repos
func (s *apiServer) handleRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	repos := make([]RepoInfoResponse, 0, len(s.cfg.Workspace.Repos))
	for _, r := range s.cfg.Workspace.Repos {
		branch := r.Branch
		if branch == "" {
			branch = "master"
		}
		// Strip credentials from URL before returning to caller.
		repoURL := r.URL
		if parsed, err := url.Parse(r.URL); err == nil {
			parsed.User = nil
			repoURL = parsed.String()
		}
		repos = append(repos, RepoInfoResponse{
			Name:      r.Name,
			Branch:    branch,
			Role:      r.Role,
			URL:       repoURL,
			LocalPath: filepath.Join(s.cfg.Workspace.Dir, r.Name),
		})
	}
	jsonOK(w, map[string]any{
		"repos": repos,
		"hint":  "Use 'name' as source_repo or branches[].repo when submitting tasks. Use 'branch' as the base branch reference.",
	})
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
	// Parse path: /api/v1/tasks/{id}[/sub]
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
		s.getTask(w, r, id, "")
	case (sub == "chain" || sub == "e2e" || sub == "ut") && r.Method == http.MethodGet:
		s.getTask(w, r, id, sub)
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

	// ── Branch-mode: auto-compute diff from NFS repo ─────────────────────────
	// When input_branch + source_repo are provided and input_diff is empty,
	// we fetch the repo and run `git diff <base>...<branch>` on the NFS clone.
	if req.InputBranch != "" && req.SourceRepo != "" && req.InputDiff == "" {
		diff, baseBranch, err := s.fetchBranchDiff(req.SourceRepo, req.InputBranch)
		if err != nil {
			jsonError(w, fmt.Sprintf("branch diff failed for %s@%s: %s", req.SourceRepo, req.InputBranch, err.Error()), http.StatusBadRequest)
			return
		}
		req.InputDiff = diff
		if req.InputDesc == "" {
			req.InputDesc = fmt.Sprintf("branch %s vs %s in %s", req.InputBranch, baseBranch, req.SourceRepo)
		}
	}

	// ── Multi-repo branches mode ──────────────────────────────────────────────
	// When branches[] is provided, fetch diffs from all repos in parallel,
	// concatenate, and use the first repo as source_repo.
	if len(req.Branches) > 0 && req.InputDiff == "" {
		type branchResult struct {
			repo      string
			branch    string
			diff      string
			baseBranch string
			err       error
		}
		results := make([]branchResult, len(req.Branches))
		var wg sync.WaitGroup
		for i, be := range req.Branches {
			wg.Add(1)
			go func(idx int, entry BranchEntry) {
				defer wg.Done()
				diff, base, err := s.fetchBranchDiff(entry.Repo, entry.Branch)
				results[idx] = branchResult{repo: entry.Repo, branch: entry.Branch, diff: diff, baseBranch: base, err: err}
			}(i, be)
		}
		wg.Wait()

		var combinedDiff strings.Builder
		var descParts []string
		var firstRepo string
		for _, res := range results {
			if res.err != nil {
				jsonError(w, fmt.Sprintf("branch diff failed for %s@%s: %s", res.repo, res.branch, res.err.Error()), http.StatusBadRequest)
				return
			}
			// Prefix each repo's diff with a header so the LLM knows which repo it belongs to.
			fmt.Fprintf(&combinedDiff, "# repo: %s  branch: %s vs %s\n", res.repo, res.branch, res.baseBranch)
			combinedDiff.WriteString(res.diff)
			combinedDiff.WriteString("\n")
			descParts = append(descParts, fmt.Sprintf("%s@%s", res.repo, res.branch))
			if firstRepo == "" {
				firstRepo = res.repo
			}
		}
		req.InputDiff = combinedDiff.String()
		if req.SourceRepo == "" {
			req.SourceRepo = firstRepo
		}
		if req.InputDesc == "" {
			req.InputDesc = "multi-repo branch analysis: " + strings.Join(descParts, ", ")
		}
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

	// Resolve modes: use request modes or fall back to server defaults.
	modes := req.Modes
	if len(modes) == 0 {
		modes = s.cfg.Server.DefaultModes
	}

	cacheKey := cache.CacheKey(req.InputDiff+req.InputDesc, []string{s.cfg.Workspace.Dir, req.SourceRepo})

	ctx := r.Context()
	task, err := s.store.CreateTask(ctx, inputType, req.InputDiff, req.InputDesc, cacheKey, req.SourceRepo, modes)
	if err != nil {
		jsonError(w, "failed to create task: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Launch analysis in background (semaphore controls concurrency).
	go s.runAnalysis(task.ID, req.InputDiff, req.InputDesc, cacheKey, req.SourceRepo, modes)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(TaskResponse{ //nolint:errcheck
		ID:         task.ID,
		Status:     string(task.Status),
		InputType:  string(task.InputType),
		SourceRepo: task.SourceRepo,
		Modes:      task.Modes,
		CreatedAt:  task.CreatedAt,
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
		resp := TaskResponse{
			ID:            t.ID,
			Status:        string(t.Status),
			InputType:     string(t.InputType),
			SourceRepo:    t.SourceRepo,
			Modes:         t.Modes,
			CreatedAt:     t.CreatedAt,
			CompletedAt:   t.CompletedAt,
			QueuePosition: t.QueuePosition,
		}
		// Inject live progress for running tasks.
		if t.Status == storage.TaskStatusRunning {
			resp.Progress = s.getProgress(t.ID)
		}
		responses = append(responses, resp)
	}
	jsonOK(w, responses)
}

// getTask retrieves a task, optionally filtered to a specific mode view.
// mode: "" (full), "chain", "e2e", "ut"
func (s *apiServer) getTask(w http.ResponseWriter, r *http.Request, id string, mode string) {
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
		ID:            task.ID,
		Status:        string(task.Status),
		InputType:     string(task.InputType),
		SourceRepo:    task.SourceRepo,
		Modes:         task.Modes,
		CreatedAt:     task.CreatedAt,
		CompletedAt:   task.CompletedAt,
		QueuePosition: task.QueuePosition,
	}

	// Inject live progress for running tasks.
	if task.Status == storage.TaskStatusRunning {
		resp.Progress = s.getProgress(id)
	}

	if task.Status == storage.TaskStatusCompleted {
		result, err := s.store.GetResult(ctx, id)
		if err == nil {
			resp.TokenUsage = result.TokenUsage
			resp.StepCount = result.StepCount
			resp.ImpactSummary = result.ImpactSummary
			resp.CrossRepoHops = result.CrossRepoHops
			resp.Risk = result.Risk

			// Mode-filtered views
			switch mode {
			case "chain":
				// Only call chain and entry points.
				var callChain any
				if len(result.CallChain) > 0 {
					if err := json.Unmarshal(result.CallChain, &callChain); err == nil {
						resp.CallChain = callChain
					}
				}
				var entryPoints any
				if len(result.EntryPoints) > 0 {
					if err := json.Unmarshal(result.EntryPoints, &entryPoints); err == nil {
						resp.EntryPoints = entryPoints
					}
				}
			case "e2e":
				// Entry points + test scenarios (FunctionAnalyses contains scenarios).
				var entryPoints any
				if len(result.EntryPoints) > 0 {
					if err := json.Unmarshal(result.EntryPoints, &entryPoints); err == nil {
						resp.EntryPoints = entryPoints
					}
				}
				var funcAnalyses any
				if len(result.FunctionAnalyses) > 0 {
					if err := json.Unmarshal(result.FunctionAnalyses, &funcAnalyses); err == nil {
						resp.FunctionAnalyses = funcAnalyses
					}
				}
				resp.ImpactSummary = result.ImpactSummary
			case "ut":
				// UT suggestions only.
				resp.UTSuggestions = result.UTSuggestions
				var funcAnalyses any
				if len(result.FunctionAnalyses) > 0 {
					if err := json.Unmarshal(result.FunctionAnalyses, &funcAnalyses); err == nil {
						resp.FunctionAnalyses = funcAnalyses
					}
				}
			default:
				// Full result (mode == "").
				var callChain any
				if len(result.CallChain) > 0 {
					if err := json.Unmarshal(result.CallChain, &callChain); err == nil {
						resp.CallChain = callChain
					}
				}
				var entryPoints any
				if len(result.EntryPoints) > 0 {
					if err := json.Unmarshal(result.EntryPoints, &entryPoints); err == nil {
						resp.EntryPoints = entryPoints
					}
				}
				resp.UTSuggestions = result.UTSuggestions
				var funcAnalyses any
				if len(result.FunctionAnalyses) > 0 {
					if err := json.Unmarshal(result.FunctionAnalyses, &funcAnalyses); err == nil {
						resp.FunctionAnalyses = funcAnalyses
					}
				}
				var indexCoverage any
				if len(result.IndexCoverage) > 0 {
					if err := json.Unmarshal(result.IndexCoverage, &indexCoverage); err == nil {
						resp.IndexCoverage = indexCoverage
					}
				}
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

// getProgress returns the current step count for a running task.
func (s *apiServer) getProgress(taskID string) int {
	s.progressMu.RLock()
	defer s.progressMu.RUnlock()
	if s.progress == nil {
		return 0
	}
	return s.progress[taskID]
}

// setProgress updates the step count for a running task.
func (s *apiServer) setProgress(taskID string, steps int) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	if s.progress == nil {
		s.progress = make(map[string]int)
	}
	s.progress[taskID] = steps
}

// clearProgress removes progress tracking for a finished task.
func (s *apiServer) clearProgress(taskID string) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	if s.progress != nil {
		delete(s.progress, taskID)
	}
}

// runAnalysis runs the analysis respecting the semaphore concurrency limit.
func (s *apiServer) runAnalysis(taskID, inputDiff, inputDesc, cacheKey, sourceRepo string, modes []string) {
	ctx := context.Background()
	log := logger.S()

	// Increment queue counter while waiting for semaphore.
	s.queueCounter.Add(1)
	s.semaphore <- struct{}{}
	s.queueCounter.Add(-1)
	defer func() { <-s.semaphore }()

	defer s.clearProgress(taskID)

	// ── Sync workspace repos before analysis ─────────────────────────────────
	// Always pull master for all configured repos so the NFS clone is fresh.
	// A quick `git pull --ff-only` on each repo takes ~1s and ensures the
	// call-chain search operates on the latest code.
	wsRepos := make([]workspace.RepoConfig, 0, len(s.cfg.Workspace.Repos))
	for _, r := range s.cfg.Workspace.Repos {
		wsRepos = append(wsRepos, workspace.RepoConfig{
			Name:   r.Name,
			URL:    r.URL,
			Branch: r.Branch,
		})
	}
	if len(wsRepos) > 0 {
		syncResults := workspace.SyncAll(ctx, s.cfg.Workspace.Dir, wsRepos)
		for name, res := range syncResults {
			if res.Err != nil {
				log.Warnw("workspace sync failed (continuing)", "repo", name, "err", res.Err)
			} else {
				log.Debugw("workspace synced", "repo", name, "commit", res.CommitHash)
			}
		}
	}

	// Check cache first.
	if cached, ok := s.cache.Get(ctx, cacheKey); ok {
		_ = s.store.SaveResult(ctx, &storage.TaskResult{
			TaskID:      taskID,
			CallChain:   cached.CallChain,
			EntryPoints: cached.EntryPoints,
			Modes:       modes,
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
		log.Errorw("checkpoint init failed", "task_id", taskID, "err", err)
		_ = s.store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusFailed)
		return
	}

	// Build contract hints from config.
	var contractHints []string
	for _, entry := range s.cfg.Contracts {
		hint := fmt.Sprintf("%s (%s) → %s (%s)",
			entry.Provider.Repo, entry.Provider.Path,
			entry.Consumer.Repo, entry.Consumer.Path,
		)
		contractHints = append(contractHints, hint)
	}

	orch := agent.NewOrchestrator(llmClient, tools, repos, s.cfg.Workspace.Dir, cp)
	if len(contractHints) > 0 {
		orch.SetContractHints(contractHints)
	}
	if s.cfg.IndexMode != "" && s.cfg.IndexMode != "off" {
		orch.SetIndexMode(s.cfg.IndexMode)
	}

	output, err := orch.Run(ctx, agent.AnalysisInput{
		Diff:        inputDiff,
		Description: inputDesc,
		SourceRepo:  sourceRepo,
		Modes:       modes,
	})
	if err != nil {
		log.Errorw("analysis failed", "task_id", taskID, "err", err)
		_ = s.store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusFailed)
		return
	}

	// Collect UT suggestions and entry scenarios from all worker outputs.
	var utSuggestions strings.Builder
	var impactSummary strings.Builder
	// Accumulate all UTAnalyses for JSONB storage.
	type repoUTAnalyses struct {
		Repo    string             `json:"repo"`
		Entries []agent.UTAnalysis `json:"entries"`
	}
	var utAnalysesList []repoUTAnalyses

	for repoName, wo := range output.WorkerOutputs {
		if wo == nil {
			continue
		}
		// UT suggestions: render each analysis as a structured Markdown table.
		for _, ua := range wo.UTAnalyses {
			fmt.Fprintf(&utSuggestions, "### %s::%s\n", repoName, ua.FuncName)
			if ua.FilePath != "" {
				fmt.Fprintf(&utSuggestions, "文件: %s\n", ua.FilePath)
			}
			if ua.Summary != "" {
				fmt.Fprintf(&utSuggestions, "%s\n", ua.Summary)
			}
			if len(ua.Constraints) > 0 {
				fmt.Fprintf(&utSuggestions, "约束: %s\n", strings.Join(ua.Constraints, "; "))
			}
			if len(ua.ExistingTests) > 0 {
				fmt.Fprintf(&utSuggestions, "已有测试: %s\n", strings.Join(ua.ExistingTests, ", "))
			}
			if len(ua.Scenarios) > 0 {
				fmt.Fprintf(&utSuggestions, "\n| 优先级 | 类型 | 场景描述 | Mock 设置 | 断言 |\n")
				fmt.Fprintf(&utSuggestions, "|--------|------|---------|----------|------|\n")
				for _, sc := range ua.Scenarios {
					fmt.Fprintf(&utSuggestions, "| %s | %s | %s | %s | %s |\n",
						sc.Priority, sc.Type, sc.Description, sc.MockSetup, sc.Assertions)
				}
			}
			utSuggestions.WriteByte('\n')
		}
		if len(wo.UTAnalyses) > 0 {
			utAnalysesList = append(utAnalysesList, repoUTAnalyses{Repo: repoName, Entries: wo.UTAnalyses})
		}

		// Impact summary: entry scenarios — full structured table.
		for _, sc := range wo.EntryScenarios {
			fmt.Fprintf(&impactSummary, "## [%s] %s\n", repoName, sc.EntryFunction)
			if sc.EntryFile != "" {
				fmt.Fprintf(&impactSummary, "文件: %s\n", sc.EntryFile)
			}
			if len(sc.ChangedVia) > 0 {
				fmt.Fprintf(&impactSummary, "变更路径: %s\n", strings.Join(sc.ChangedVia, " → "))
			}
			if len(sc.Preconditions) > 0 {
				fmt.Fprintf(&impactSummary, "前置条件: %s\n", strings.Join(sc.Preconditions, "; "))
			}
			if sc.TypicalInputs != "" {
				fmt.Fprintf(&impactSummary, "典型入参: %s\n", sc.TypicalInputs)
			}
			if len(sc.Scenarios) > 0 {
				fmt.Fprintf(&impactSummary, "\n| 优先级 | 类型 | 场景描述 | 关键入参 | 预期结果 | 观察点 Oracle |\n")
				fmt.Fprintf(&impactSummary, "|--------|------|---------|---------|---------|---------------|\n")
				for _, ts := range sc.Scenarios {
					oracles := "-"
					if len(ts.Oracles) > 0 {
						parts := make([]string, 0, len(ts.Oracles))
						for _, o := range ts.Oracles {
							parts = append(parts, fmt.Sprintf("[%s] %s → %s", o.Type, o.Target, o.Assertion))
						}
						oracles = strings.Join(parts, "<br>")
					}
					fmt.Fprintf(&impactSummary, "| %s | %s | %s | %s | %s | %s |\n",
						ts.Priority, ts.Type, ts.Description, ts.Input, ts.Expected, oracles)
				}
			}
			impactSummary.WriteByte('\n')
		}
	}

	callChainJSON, _ := json.Marshal(output.CallGraph)
	entryPointsJSON, _ := json.Marshal(output.EntryPoints)

	// Merge FunctionAnalyses from orchestrator output + UTAnalyses for JSONB storage.
	type combinedAnalyses struct {
		FunctionAnalyses []agent.FunctionAnalysis `json:"function_analyses,omitempty"`
		UTAnalyses       []repoUTAnalyses         `json:"ut_analyses,omitempty"`
	}
	funcAnalysesJSON, _ := json.Marshal(combinedAnalyses{
		FunctionAnalyses: output.FunctionAnalyses,
		UTAnalyses:       utAnalysesList,
	})

	// Risk and index coverage from graph analysis.
	risk := output.Risk
	crossRepoHops := len(output.CrossRepoHops)
	indexCoverageJSON, _ := json.Marshal(output.IndexCoverage)

	_ = s.store.SaveResult(ctx, &storage.TaskResult{
		TaskID:           taskID,
		CallChain:        callChainJSON,
		EntryPoints:      entryPointsJSON,
		FunctionAnalyses: funcAnalysesJSON,
		UTSuggestions:    utSuggestions.String(),
		ImpactSummary:    impactSummary.String(),
		CrossRepoHops:    crossRepoHops,
		Risk:             risk,
		IndexCoverage:    indexCoverageJSON,
		Modes:            modes,
	})
	_ = s.store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusCompleted)

	cacheResult := &cache.AnalysisResult{
		TaskID:      taskID,
		CallChain:   callChainJSON,
		EntryPoints: entryPointsJSON,
		CreatedAt:   time.Now(),
	}
	_ = s.cache.Set(ctx, cacheKey, cacheResult, 0)

	log.Infow("analysis completed",
		"task_id", taskID,
		"nodes", len(output.CallGraph),
		"entry_points", len(output.EntryPoints),
		"modes", modes,
	)
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

// configRepos builds RepoInfo list from workspace config.
// Each configured repo maps to a local clone path under workspace.dir.
func configRepos(cfg *config.Config) []agent.RepoInfo {
	if cfg.Workspace.Dir == "" {
		return nil
	}
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
	// Fallback: single workspace dir as generic "workspace" repo.
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

// ---------------------------------------------------------------------------
// storageTaskAdapter adapts *storage.Store to webhook.TaskCreator.
// The webhook handler uses the old 5-arg signature; we fill sourceRepo=""
// and modes=nil (defaults to all) for webhook-triggered tasks.
// ---------------------------------------------------------------------------

type storageTaskAdapter struct {
	store *storage.Store
}

func (a *storageTaskAdapter) CreateTask(ctx context.Context, inputType, inputDiff, inputDesc, cacheKey string) (webhook.TaskRecord, error) {
	task, err := a.store.CreateTask(ctx, storage.InputType(inputType), inputDiff, inputDesc, cacheKey, "", nil)
	if err != nil {
		return webhook.TaskRecord{}, err
	}
	return webhook.TaskRecord{ID: task.ID}, nil
}

// ---------------------------------------------------------------------------
// fetchBranchDiff fetches the remote branch and returns the unified diff
// between the repo's base branch (master/main) and the feature branch.
//
// It operates on the NFS clone at <workspace.dir>/<repoName>.
// Returns the diff string and the base branch name used.
// ---------------------------------------------------------------------------

func (s *apiServer) fetchBranchDiff(repoName, featureBranch string) (diff, baseBranch string, err error) {
	repoDir := filepath.Join(s.cfg.Workspace.Dir, repoName)
	if _, statErr := os.Stat(filepath.Join(repoDir, ".git")); os.IsNotExist(statErr) {
		return "", "", fmt.Errorf("repo %q not found on NFS workspace (%s) — run `shirakami workspace sync` first", repoName, repoDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 1. Fetch all remote refs (including the feature branch).
	fetchCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "--depth=50", "origin", featureBranch)
	if out, ferr := fetchCmd.CombinedOutput(); ferr != nil {
		return "", "", fmt.Errorf("git fetch origin %s: %w\n%s", featureBranch, ferr, out)
	}

	// 2. Determine base branch: use the repo's configured branch, default to "master".
	baseBranch = "master"
	for _, r := range s.cfg.Workspace.Repos {
		if r.Name == repoName && r.Branch != "" {
			baseBranch = r.Branch
			break
		}
	}

	// 3. Compute three-dot diff: changes on featureBranch not yet in baseBranch.
	//    Using FETCH_HEAD (just fetched) vs origin/<baseBranch> keeps it self-contained.
	diffRef := fmt.Sprintf("origin/%s...FETCH_HEAD", baseBranch)
	diffCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "diff", diffRef)
	out, derr := diffCmd.Output()
	if derr != nil {
		// Exit code 1 from git diff means non-empty diff — that's fine.
		// Any other error is a real failure.
		if exitErr, ok := derr.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			return "", baseBranch, fmt.Errorf("git diff %s: %w", diffRef, derr)
		}
	}

	diffStr := strings.TrimSpace(string(out))
	if diffStr == "" {
		return "", baseBranch, fmt.Errorf("branch %q has no diff against %s/%s — nothing to analyze", featureBranch, repoName, baseBranch)
	}
	return diffStr, baseBranch, nil
}
