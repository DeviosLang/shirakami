package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
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
	"github.com/DeviosLang/shirakami/internal/index"
	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/logger"
	"github.com/DeviosLang/shirakami/internal/memory"
	"github.com/DeviosLang/shirakami/internal/storage"
	itool "github.com/DeviosLang/shirakami/internal/tool"
	itrace "github.com/DeviosLang/shirakami/internal/trace"
	"github.com/DeviosLang/shirakami/internal/webhook"
	"github.com/DeviosLang/shirakami/internal/workspace"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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

	// Initialise OpenTelemetry tracing provider.
	// Uses OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_STDOUT_TRACE env vars;
	// falls back to no-op when neither is set.
	otelShutdown, otelErr := itrace.InitProvider("shirakami", version)
	if otelErr != nil {
		log.Sugar().Warnw("otel.init_failed", "err", otelErr)
	} else {
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = otelShutdown(shutCtx)
		}()
	}

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

	// Start config hot-reload watcher. Only safe fields (LLM params,
	// server.max_concurrent_analyses, server.default_modes) are reloaded;
	// fields like db.dsn and redis.addr require a server restart.
	watcher, watchErr := config.NewWatcher(func(updated *config.Config) {
		// Caller is responsible for acting on updated safe fields.
		// For now we just log; downstream components can be extended here.
		_ = updated
	})
	if watchErr != nil {
		log.Sugar().Warnw("config.watcher_failed", "err", watchErr)
	} else {
		defer watcher.Stop()
	}

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
		metrics:      feedback.NewMetrics(),
		semaphore:    make(chan struct{}, maxConcurrent),
		queueCounter: new(atomic.Int64),
	}

	// Initialize Pushgateway pusher when configured.
	if cfg.Metrics.PushgatewayURL != "" {
		interval := time.Duration(cfg.Metrics.PushIntervalSeconds) * time.Second
		pusher, pushErr := feedback.NewPusher(cfg.Metrics.PushgatewayURL, cfg.Metrics.JobName, interval)
		if pushErr != nil {
			// Non-fatal: log and continue without pushing.
			log.Sugar().Warnw("pushgateway init failed", "err", pushErr)
		} else {
			srv.pusher = pusher
			pusher.Start()
			defer pusher.Stop(ctx)
		}
	}

	// L2: clean up any worktrees left by a previous crashed process.
	// NFS PVC persists across pod restarts, so this is mandatory (not just best-effort).
	workspace.CleanupWorktrees(cfg.Workspace.Dir, 0)

	// Backfill .git/info/attributes for repos already cloned on the NFS workspace
	// (e.g. repos cloned before this feature was added). This is idempotent.
	for _, r := range cfg.Workspace.Repos {
		workspace.WriteGitInfoAttributes(filepath.Join(cfg.Workspace.Dir, r.Name))
	}

	// L3: periodic GC — removes worktrees older than 2 hours (defense in depth).
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			workspace.CleanupWorktrees(cfg.Workspace.Dir, 2*time.Hour)
		}
	}()

	// L4: nightly symbol index rebuild at 04:00 CST.
	// Only runs when IndexMode is not "off" and a DB pool is available.
	if cfg.IndexMode != "" && cfg.IndexMode != "off" && pool != nil {
		go srv.scheduleNightlyIndex(ctx)
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
				go srv.runAnalysis(taskID, inputDiff, inputDesc, cacheKey, "", cfg.Server.DefaultModes, "", nil)
			},
		},
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.Handle("/metrics", feedback.Handler())
	mux.HandleFunc("/api/v1/repos", srv.handleRepos)
	mux.HandleFunc("/api/v1/repos/register", srv.handleReposRegister)
	mux.HandleFunc("/api/v1/tasks", srv.handleTasks)
	mux.HandleFunc("/api/v1/tasks/", srv.handleTaskByID)
	mux.HandleFunc("/api/v1/cache", srv.handleCache)
	mux.Handle("/api/v1/webhook", webhookHandler)

	// Recover orphaned tasks: any task left in "running" state from a previous
	// server run (crash or pod restart) will never complete on their own.
	// Reset them to "pending" and re-launch their analysis goroutines so they
	// run again automatically — callers do not need to resubmit.
	if orphans, reqErr := store.RequeueOrphanedTasks(ctx); reqErr != nil {
		log.Sugar().Warnw("orphan requeue failed", "err", reqErr)
	} else if len(orphans) > 0 {
		log.Sugar().Infow("orphaned tasks requeued", "count", len(orphans))
		for _, t := range orphans {
			t := t // capture
			go srv.runAnalysis(t.ID, t.InputDiff, t.InputDesc, t.CacheKey, t.SourceRepo, t.Modes, "", nil)
		}
	}

	log.Sugar().Infof("listening on %s", listenAddr)
	// Wrap mux with OTel HTTP instrumentation so every request gets a root span
	// carrying the HTTP method, route, and status code attributes.
	return http.ListenAndServe(listenAddr, otelhttp.NewHandler(mux, "shirakami"))
}

// ---------------------------------------------------------------------------
// API server
// ---------------------------------------------------------------------------

type apiServer struct {
	cfg   *config.Config
	store *storage.Store
	pool  *pgxpool.Pool
	cache *cache.Cache

	// metrics is the Prometheus metrics sink shared across all analysis runs.
	metrics *feedback.Metrics

	// pusher is the optional Pushgateway client. When non-nil, metrics are
	// pushed to the Pushgateway after each task completes and periodically.
	pusher *feedback.Pusher

	// semaphore limits concurrent analysis jobs.
	// Capacity == cfg.Server.MaxConcurrentAnalyses (default 1).
	semaphore chan struct{}

	// queueCounter tracks the number of analyses waiting in queue.
	queueCounter *atomic.Int64

	// cacheHits and cacheTotal are used to compute the cache-hit ratio gauge.
	// Incremented atomically; ratio is updated each time runAnalysis is called.
	cacheHits  atomic.Int64
	cacheTotal atomic.Int64

	// progressMu protects the progress map.
	progressMu sync.RWMutex
	// progress maps taskID → step count for in-flight analyses.
	progress map[string]int

	// repoFetchMu serialises concurrent git-fetch operations per repository.
	// Key: repo name string → *sync.Mutex.
	// Using sync.Map avoids a global lock on the map itself while still
	// preventing two goroutines from running git-fetch on the same repo
	// concurrently (which causes "shallow.lock: File exists" failures).
	repoFetchMu sync.Map
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

	// ExtraPrompt is optional business-context text injected into the e2e scenario
	// and UT follow-up prompts. Use it to improve accuracy when domain knowledge
	// cannot be inferred from code alone.
	// Example: "This service uses SM4 encryption; always verify the key-loading path."
	ExtraPrompt string `json:"extra_prompt,omitempty"`
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

	// Warnings contains human-readable diagnostic hints when the result is empty.
	// For example: diff has hunks but no function names were detected.
	Warnings []string `json:"warnings,omitempty"`
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

// handleReposRegister resolves one or more git repository URLs and returns
// structured RepoInfo for each — including the canonical name derived from the
// URL, the inferred default branch, and whether the repo is already registered
// in the server config.
//
// POST /api/v1/repos/register
//
//	{"urls": ["https://gitlab.example.com/cvm/vstation_compute.git",
//	           "https://gitlab.example.com/cvm/vstation_api.git"]}
//
// A single URL may be passed as a convenience:
//
//	{"url": "https://gitlab.example.com/cvm/vstation_compute.git"}
//
// Response:
//
//	{"repos": [
//	  {"url": "...", "name": "vstation_compute", "branch": "master",
//	   "registered": true, "local_path": "/workspace/vstation_compute"}
//	]}
func (s *apiServer) handleReposRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		URL  string   `json:"url"`  // single-URL convenience field
		URLs []string `json:"urls"` // batch
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Merge single + batch into one list.
	rawURLs := body.URLs
	if body.URL != "" {
		rawURLs = append(rawURLs, body.URL)
	}
	if len(rawURLs) == 0 {
		jsonError(w, "provide at least one URL in 'url' or 'urls'", http.StatusBadRequest)
		return
	}

	// Build a quick lookup of already-registered repos.
	// Key is a normalized form of the URL: scheme-less, credential-less, .git-stripped,
	// lower-cased host — so that http/https/SSH variants all match the same entry.
	// e.g. "http://oauth2:token@git.woa.com/vstation/api.git"
	//      "https://git.woa.com/vstation/api"
	//      "git@git.woa.com:vstation/api"
	// all normalize to "git.woa.com/vstation/api".
	normalizeRepoURL := func(raw string) string {
		raw = strings.TrimSpace(raw)
		// SSH format: git@host:path[.git]
		if strings.HasPrefix(raw, "git@") {
			rest := raw[len("git@"):]
			if idx := strings.LastIndex(rest, "@"); idx >= 0 {
				rest = rest[idx+1:]
			}
			rest = strings.Replace(rest, ":", "/", 1) // host:path → host/path
			rest = strings.TrimSuffix(rest, ".git")
			return strings.ToLower(rest)
		}
		// HTTP(S): strip scheme, credentials, .git
		if parsed, err := url.Parse(raw); err == nil {
			parsed.User = nil
			parsed.Scheme = ""
			p := strings.TrimPrefix(parsed.String(), "//")
			p = strings.TrimSuffix(p, ".git")
			return strings.ToLower(p)
		}
		return strings.ToLower(raw)
	}

	type registeredEntry struct {
		name      string
		branch    string
		localPath string
	}
	registered := make(map[string]registeredEntry, len(s.cfg.Workspace.Repos))
	for _, r := range s.cfg.Workspace.Repos {
		key := normalizeRepoURL(r.URL)
		branch := r.Branch
		if branch == "" {
			branch = "master"
		}
		registered[key] = registeredEntry{
			name:      r.Name,
			branch:    branch,
			localPath: path.Join(s.cfg.Workspace.Dir, r.Name),
		}
	}

	type repoResult struct {
		URL        string `json:"url"`
		Name       string `json:"name"`
		Branch     string `json:"branch"`
		Registered bool   `json:"registered"`
		LocalPath  string `json:"local_path,omitempty"`
		Error      string `json:"error,omitempty"`
	}

	results := make([]repoResult, len(rawURLs))
	var wg sync.WaitGroup

	for i, raw := range rawURLs {
		i, raw := i, raw
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := repoResult{URL: raw}

			parsed, err := url.Parse(raw)
			if err != nil {
				res.Error = "invalid URL: " + err.Error()
				results[i] = res
				return
			}

			// Derive canonical name: last path segment, strip .git suffix.
			// e.g. "gitlab.com/cvm/vstation_compute.git" → "vstation_compute"
			segment := path.Base(parsed.Path)
			segment = strings.TrimSuffix(segment, ".git")
			if segment == "" || segment == "." {
				res.Error = "cannot derive repo name from URL path"
				results[i] = res
				return
			}
			res.Name = segment

			// Strip credentials for the lookup key.
			parsed.User = nil
			cleanURL := parsed.String()

			// Check if already registered (normalized match: scheme/credentials/.git agnostic).
			normKey := normalizeRepoURL(raw)
			if entry, ok := registered[normKey]; ok {
				res.Registered = true
				res.Branch = entry.branch
				res.LocalPath = entry.localPath
				// Use the registered canonical name (operator may have overridden it).
				res.Name = entry.name
				results[i] = res
				return
			}
			_ = cleanURL // kept for potential future use

			// Not registered in shirakami workspace — return an error so callers
			// can skip this repo instead of submitting with a wrong inferred name.
			res.Error = "repo not registered in shirakami workspace; add it to shirakami.yaml and run workspace sync"
			res.Name = ""
			results[i] = res
		}()
	}
	wg.Wait()

	jsonOK(w, map[string]any{"repos": results})
}

// probeDefaultBranch runs `git ls-remote --symref <url> HEAD` to read the
// remote's default branch without cloning. Falls back to "master" on any error.
// The URL may contain credentials (https://user:token@host/…) so they are
// forwarded verbatim to git; they are never logged or returned to the caller.
func probeDefaultBranch(ctx context.Context, repoURL string) string {
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--symref", repoURL, "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "master"
	}
	// Output looks like:
	//   ref: refs/heads/main	HEAD
	//   <sha>	HEAD
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") {
			// "ref: refs/heads/main\tHEAD" → "main"
			branch := strings.TrimPrefix(line, "ref: refs/heads/")
			branch = strings.Fields(branch)[0]
			if branch != "" {
				return branch
			}
		}
	}
	return "master"
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
	case sub == "cache" && r.Method == http.MethodDelete:
		s.deleteTaskCache(w, r, id)
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
			errMsg := fmt.Sprintf("branch diff failed for %s@%s: %s", req.SourceRepo, req.InputBranch, err.Error())
			s.recordFailedSubmit(r.Context(), req, errMsg)
			jsonError(w, errMsg, http.StatusBadRequest)
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
	var submitWarnings []string // repos skipped due to branch-not-found
	if len(req.Branches) > 0 && req.InputDiff == "" {
		type branchResult struct {
			repo       string
			branch     string
			diff       string
			baseBranch string
			err        error
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
				var notFound *errBranchNotFound
				var emptyDiff *errEmptyDiff
				if errors.As(res.err, &notFound) {
					// Branch doesn't exist on remote — skip this repo and warn.
					// The task still runs for the remaining repos.
					warnMsg := fmt.Sprintf("branch %q not found for repo %q — skipped (no diff contribution)", res.branch, res.repo)
					submitWarnings = append(submitWarnings, warnMsg)
					logger.S().Warnw("fetchBranchDiff.branch_not_found_skipped",
						"repo", res.repo, "branch", res.branch)
					continue
				}
				if errors.As(res.err, &emptyDiff) {
					// Branch exists but has no diff (already merged / squash-merged).
					// Skip gracefully — the remaining repos still get analysed.
					warnMsg := fmt.Sprintf("branch %q for repo %q has empty diff (already merged?) — skipped", res.branch, res.repo)
					submitWarnings = append(submitWarnings, warnMsg)
					logger.S().Warnw("fetchBranchDiff.empty_diff_skipped",
						"repo", res.repo, "branch", res.branch, "detail", emptyDiff.Detail)
					continue
				}
				errMsg := fmt.Sprintf("branch diff failed for %s@%s: %s", res.repo, res.branch, res.err.Error())
				s.recordFailedSubmit(r.Context(), req, errMsg)
				jsonError(w, errMsg, http.StatusBadRequest)
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
		if len(descParts) == 0 {
			// Every repo was skipped (all branches missing). Nothing to analyse.
			errMsg := "all branches not found on remote: " + strings.Join(submitWarnings, "; ")
			s.recordFailedSubmit(r.Context(), req, errMsg)
			jsonError(w, errMsg, http.StatusBadRequest)
			return
		}
		req.InputDiff = combinedDiff.String()
		if req.SourceRepo == "" {
			req.SourceRepo = firstRepo
		}
		if req.InputDesc == "" {
			req.InputDesc = "multi-repo branch analysis: " + strings.Join(descParts, ", ")
		}
		// Attach skip warnings to ExtraPrompt so the LLM is aware.
		if len(submitWarnings) > 0 {
			note := "\n[NOTE: the following repos were skipped because their branch was not found on remote: " +
				strings.Join(submitWarnings, "; ") + "]"
			req.ExtraPrompt += note
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
	// activeBranches: prefer multi-repo branches list; fall back to single InputBranch wrap.
	activeBranches := req.Branches
	if len(activeBranches) == 0 && req.InputBranch != "" && req.SourceRepo != "" {
		activeBranches = []BranchEntry{{Repo: req.SourceRepo, Branch: req.InputBranch}}
	}
	go s.runAnalysis(task.ID, req.InputDiff, req.InputDesc, cacheKey, req.SourceRepo, modes, req.ExtraPrompt, activeBranches)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(TaskResponse{ //nolint:errcheck
		ID:         task.ID,
		Status:     string(task.Status),
		InputType:  string(task.InputType),
		SourceRepo: task.SourceRepo,
		Modes:      task.Modes,
		CreatedAt:  task.CreatedAt,
		Warnings:   submitWarnings,
	})
}

// recordFailedSubmit creates a failed task record in the DB so that pre-analysis
// errors (e.g. git fetch failures) are visible in task history and queryable via
// GET /tasks. It is best-effort: logging on error rather than surfacing to the caller.
func (s *apiServer) recordFailedSubmit(ctx context.Context, req SubmitTaskRequest, errMsg string) {
	inputType := storage.InputType(req.InputType)
	if inputType == "" {
		inputType = storage.InputTypeDiff
	}
	modes := req.Modes
	if len(modes) == 0 {
		modes = s.cfg.Server.DefaultModes
	}
	// Use the raw request diff (may be empty for branch-mode failures).
	cacheKey := cache.CacheKey(req.InputDiff+req.InputDesc, []string{s.cfg.Workspace.Dir, req.SourceRepo})
	task, err := s.store.CreateTask(ctx, inputType, req.InputDiff, req.InputDesc, cacheKey, req.SourceRepo, modes)
	if err != nil {
		logger.S().Warnw("recordFailedSubmit.create_task_failed", "err", err)
		return
	}
	if err := s.store.UpdateTaskStatusWithError(ctx, task.ID, errMsg); err != nil {
		logger.S().Warnw("recordFailedSubmit.update_failed", "task_id", task.ID, "err", err)
	}
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

			// Inject diagnostic warnings when result is empty.
			if isEmptyResult(result) {
				resp.Warnings = buildEmptyResultWarnings(task.InputDiff)
			} else {
				// Inject ghost-file warnings when LLM hallucinated file paths.
				resp.Warnings = append(resp.Warnings, extractGhostWarnings(result.ImpactSummary)...)
			}
		}
	}

	// Inject failure warnings when task failed.
	if task.Status == storage.TaskStatusFailed {
		resp.Warnings = buildFailedWarnings(task.ErrorMsg)
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

	// Update false-positive-rate gauge asynchronously — non-blocking for the HTTP response.
	if s.metrics != nil && s.pool != nil {
		go func() {
			// Count all feedback and false_positive feedback over the last 30 days.
			var total, fp int
			qCtx := context.Background()
			row := s.pool.QueryRow(qCtx,
				`SELECT COUNT(*) FROM feedback WHERE created_at >= NOW() - INTERVAL '30 days'`,
			)
			if err := row.Scan(&total); err == nil && total > 0 {
				row2 := s.pool.QueryRow(qCtx,
					`SELECT COUNT(*) FROM feedback WHERE type = 'false_positive' AND created_at >= NOW() - INTERVAL '30 days'`,
				)
				if err2 := row2.Scan(&fp); err2 == nil {
					s.metrics.SetFalsePositiveRate(float64(fp) / float64(total))
				}
			}
		}()
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
// activeBranches lists the {repo, branch} pairs whose worktrees should be created
// before analysis so ripgrep searches the feature-branch code (not just master).
// Pass nil when no branch input is available (bare diff / orphan requeue).
func (s *apiServer) runAnalysis(taskID, inputDiff, inputDesc, cacheKey, sourceRepo string, modes []string, extraPrompt string, activeBranches []BranchEntry) {
	ctx := context.Background()

	// Root span for the complete analysis lifecycle (semaphore wait → result persist).
	ctx, rootSpan := itrace.Start(ctx, itrace.OpAnalysisRun,
		attribute.String(itrace.AttrTaskID, taskID),
		attribute.String(itrace.AttrSourceRepo, sourceRepo),
	)
	defer rootSpan.End()

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

	// ── Worktree setup: checkout feature branches so ripgrep searches branch code ─
	// Must happen AFTER SyncAll (master pulled) so we can base worktrees on fresh refs.
	// Only created when activeBranches is non-empty (branch-mode requests).
	wtBase := filepath.Join(s.cfg.Workspace.Dir, workspace.WorktreesSubdir, taskID)
	activeWorktrees := make(map[string]string) // repoName → worktreeDir

	for _, be := range activeBranches {
		repoDir := filepath.Join(s.cfg.Workspace.Dir, be.Repo)
		wtDir := filepath.Join(wtBase, be.Repo)
		ref := "origin/" + be.Branch
		wtCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		if err := workspace.CreateWorktree(wtCtx, repoDir, wtDir, ref); err != nil {
			log.Warnw("worktree.create_failed (falling back to master)",
				"task_id", taskID, "repo", be.Repo, "branch", be.Branch, "err", err)
			cancel()
			continue
		}
		cancel()
		activeWorktrees[be.Repo] = wtDir
		log.Infow("worktree.created",
			"task_id", taskID, "repo", be.Repo, "branch", be.Branch, "path", wtDir)
	}

	// Cleanup worktrees when runAnalysis exits (normal completion, error, or ctx cancellation).
	defer func() {
		for repoName, wtDir := range activeWorktrees {
			repoDir := filepath.Join(s.cfg.Workspace.Dir, repoName)
			workspace.RemoveWorktree(context.Background(), repoDir, wtDir)
			log.Infow("worktree.removed", "task_id", taskID, "repo", repoName)
		}
		_ = os.RemoveAll(wtBase)
	}()

	// Check cache first.
	s.cacheTotal.Add(1)
	if cached, ok := s.cache.Get(ctx, cacheKey); ok {
		rootSpan.SetAttributes(attribute.Bool(itrace.AttrCacheHit, true))
		s.cacheHits.Add(1)
		if s.metrics != nil {
			hits := float64(s.cacheHits.Load())
			total := float64(s.cacheTotal.Load())
			if total > 0 {
				s.metrics.SetCacheHitRatio(hits / total)
			}
			s.metrics.RecordTask("completed")
		}
		_ = s.store.SaveResult(ctx, &storage.TaskResult{
			TaskID:           taskID,
			CallChain:        cached.CallChain,
			EntryPoints:      cached.EntryPoints,
			FunctionAnalyses: cached.FunctionAnalyses,
			ImpactSummary:    cached.ImpactSummary,
			Modes:            modes,
		})
		_ = s.store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusCompleted)
		if s.pusher != nil {
			s.pusher.Push()
		}
		return
	}
	// Update cache-hit ratio after a miss (denominator already incremented above).
	if s.metrics != nil {
		hits := float64(s.cacheHits.Load())
		total := float64(s.cacheTotal.Load())
		if total > 0 {
			s.metrics.SetCacheHitRatio(hits / total)
		}
	}

	_ = s.store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusRunning)

	llmClient := llm.NewClient(llm.Config{
		BaseURL:        s.cfg.LLM.Endpoint,
		APIKey:         s.cfg.LLM.APIKey,
		Model:          s.cfg.LLM.Model,
		RequestTimeout: s.cfg.LLM.RequestTimeout,
	})

	tools := defaultTools(s.cfg.Workspace.Dir)
	repos := configRepos(s.cfg)

	// Override repo paths so orchestrator + workers search feature-branch code.
	// Repos without a worktree (no branch input or worktree creation failed) keep
	// their original master path — graceful degradation is built in.
	for i, r := range repos {
		if wtPath, ok := activeWorktrees[r.Name]; ok {
			repos[i].Path = wtPath
			log.Debugw("worktree.repo_path_overridden",
				"task_id", taskID, "repo", r.Name, "path", wtPath)
		}
	}

	cpDir := os.TempDir() + "/shirakami-checkpoints"
	cp, err := checkpoint.NewFileCheckpointer(cpDir)
	if err != nil {
		log.Errorw("checkpoint init failed", "task_id", taskID, "err", err)
		_ = s.store.UpdateTaskStatusWithError(ctx, taskID, fmt.Sprintf("checkpoint init failed: %s", err.Error()))
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
	// Apply max_rounds from config (default 3 via config default).
	// CLI fast mode uses SetMaxRounds(3); here we read the configured value directly.
	if s.cfg.MaxRounds > 0 {
		orch.SetMaxRounds(s.cfg.MaxRounds)
	}
	// Apply P1 step budget from config (default 150 via config default).
	if s.cfg.P1StepBudget > 0 {
		orch.SetP1StepBudget(s.cfg.P1StepBudget)
	}
	// Apply P0 step budget from config (default 0 = no cap).
	if s.cfg.P0StepBudget > 0 {
		orch.SetP0StepBudget(s.cfg.P0StepBudget)
	}
	if s.cfg.IndexMode != "" && s.cfg.IndexMode != "off" {
		orch.SetIndexMode(s.cfg.IndexMode)
	}
	if s.pool != nil {
		l1 := memory.NewLayer1(s.pool)
		orch.SetLayer1(l1)
	}
	if s.metrics != nil {
		orch.SetMetrics(s.metrics, s.cfg.LLM.Model)
	}

	output, err := orch.Run(ctx, agent.AnalysisInput{
		Diff:        inputDiff,
		Description: inputDesc,
		SourceRepo:  sourceRepo,
		Modes:       modes,
		ExtraPrompt: extraPrompt,
	})
	if err != nil {
		log.Errorw("analysis failed", "task_id", taskID, "err", err)
		rootSpan.RecordError(err)
		rootSpan.SetStatus(codes.Error, err.Error())
		_ = s.store.UpdateTaskStatusWithError(ctx, taskID, err.Error())
		if s.metrics != nil {
			s.metrics.RecordTask("failed")
		}
		if s.pusher != nil {
			s.pusher.Push()
		}
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

	// Append ghost-files sentinel line to ImpactSummary so it can be surfaced
	// as actionable warnings when the task result is fetched. We embed it as a
	// hidden HTML comment so it does not affect Markdown rendering.
	impactSummaryStr := impactSummary.String()
	if len(output.GhostFiles) > 0 {
		ghostJSON, _ := json.Marshal(output.GhostFiles)
		impactSummaryStr += "\n<!-- ghost_files: " + string(ghostJSON) + " -->"
	}

	_ = s.store.SaveResult(ctx, &storage.TaskResult{
		TaskID:           taskID,
		CallChain:        callChainJSON,
		EntryPoints:      entryPointsJSON,
		FunctionAnalyses: funcAnalysesJSON,
		UTSuggestions:    utSuggestions.String(),
		ImpactSummary:    impactSummaryStr,
		CrossRepoHops:    crossRepoHops,
		Risk:             risk,
		IndexCoverage:    indexCoverageJSON,
		Modes:            modes,
	})
	_ = s.store.UpdateTaskStatus(ctx, taskID, storage.TaskStatusCompleted)
	if s.metrics != nil {
		s.metrics.RecordTask("completed")
	}
	if s.pusher != nil {
		s.pusher.Push()
	}

	cacheResult := &cache.AnalysisResult{
		TaskID:           taskID,
		CallChain:        callChainJSON,
		EntryPoints:      entryPointsJSON,
		FunctionAnalyses: funcAnalysesJSON,
		ImpactSummary:    impactSummaryStr,
		CreatedAt:        time.Now(),
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v) //nolint:errcheck
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]string{"error": msg}) //nolint:errcheck
}

// ---------------------------------------------------------------------------
// Cache management handlers
// ---------------------------------------------------------------------------

// handleCache handles DELETE /api/v1/cache — clears ALL cached analysis results.
func (s *apiServer) handleCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n, err := s.cache.DeleteAll(r.Context())
	if err != nil {
		jsonError(w, "failed to clear cache: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"deleted": n})
}

// deleteTaskCache handles DELETE /api/v1/tasks/{id}/cache — clears the cached
// result for a specific task so the next identical submission triggers a fresh
// LLM analysis instead of returning the cached result.
func (s *apiServer) deleteTaskCache(w http.ResponseWriter, r *http.Request, id string) {
	task, err := s.store.GetTask(r.Context(), id)
	if err != nil {
		jsonError(w, "task not found", http.StatusNotFound)
		return
	}
	if task.CacheKey == "" {
		jsonOK(w, map[string]any{"deleted": 0, "message": "task has no cache key"})
		return
	}
	if err := s.cache.Delete(r.Context(), task.CacheKey); err != nil {
		jsonError(w, "failed to delete cache: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"deleted": 1, "cache_key": task.CacheKey})
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
// errBranchNotFound is returned by fetchBranchDiff when the remote branch does
// not exist. Callers can use errors.As to distinguish this from other failures
// and treat the repo as having no diff (skip gracefully with a warning).
type errBranchNotFound struct {
	Repo   string
	Branch string
}

func (e *errBranchNotFound) Error() string {
	return fmt.Sprintf("branch %q not found on remote for repo %q", e.Branch, e.Repo)
}

// errEmptyDiff is returned by fetchBranchDiff when the branch exists but
// produces an empty diff against the base branch (already merged / squash-merged
// / rebase-merged). Callers treat this the same as branch-not-found: skip the
// repo with a warning and continue analysing the remaining repos.
type errEmptyDiff struct {
	Repo       string
	Branch     string
	BaseBranch string
	Detail     string
}

func (e *errEmptyDiff) Error() string {
	msg := fmt.Sprintf("branch %q has no diff against %s/%s (already merged or empty)", e.Branch, e.Repo, e.BaseBranch)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
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
		// Repo not cloned yet — try to clone it on-demand from shirakami.yaml config.
		var repoURL string
		for _, r := range s.cfg.Workspace.Repos {
			if r.Name == repoName {
				repoURL = r.URL
				break
			}
		}
		if repoURL == "" {
			return "", "", fmt.Errorf("repo %q not found on NFS workspace (%s) and not configured in shirakami.yaml", repoName, repoDir)
		}
		cloneLog := logger.S()
		cloneLog.Infow("workspace.clone_on_demand", "repo", repoName, "url", repoURL)
		cloneCtx, cloneCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cloneCancel()
		cloneCmd := exec.CommandContext(cloneCtx, "git", "clone", "--depth=50", repoURL, repoDir)
		if out, cloneErr := cloneCmd.CombinedOutput(); cloneErr != nil {
			return "", "", fmt.Errorf("repo %q not found on NFS workspace and auto-clone failed: %w\n%s", repoName, cloneErr, out)
		}
		cloneLog.Infow("workspace.clone_on_demand_done", "repo", repoName)
		workspace.WriteGitInfoAttributes(repoDir)
	}

	// Serialise all git operations on this repo to prevent concurrent fetches
	// racing for the same lock files (shallow.lock, FETCH_HEAD.lock, etc.).
	// sync.Map.LoadOrStore is used to lazily create one mutex per repo name.
	muVal, _ := s.repoFetchMu.LoadOrStore(repoName, &sync.Mutex{})
	mu := muVal.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	// Remove any stale git lock files left by a previously crashed process.
	// These are safe to delete: git creates them transiently during fetch/pack
	// operations, and a leftover lock always means the prior process is gone.
	// This runs inside the per-repo mutex, so it never races with a live fetch.
	for _, lockFile := range []string{
		filepath.Join(repoDir, ".git", "shallow.lock"),
		filepath.Join(repoDir, ".git", "FETCH_HEAD.lock"),
		filepath.Join(repoDir, ".git", "index.lock"),
	} {
		if removeErr := os.Remove(lockFile); removeErr == nil {
			logger.S().Warnw("fetchBranchDiff.stale_lock_removed",
				"repo", repoName,
				"lock_file", lockFile,
			)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 1. Fetch the feature branch and explicitly map it to a remote-tracking ref
	// (refs/remotes/origin/<branch>) so that "git worktree add origin/<branch>"
	// works even when the repo was cloned with --single-branch (only master in
	// the fetch refspec). Without the explicit refspec, git fetch only updates
	// FETCH_HEAD and leaves refs/remotes/origin/<branch> missing.
	remoteRef := fmt.Sprintf("refs/remotes/origin/%s", featureBranch)
	fetchRefspec := fmt.Sprintf("%s:%s", featureBranch, remoteRef)
	fetchCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "--depth=50", "origin", fetchRefspec)
	if out, ferr := fetchCmd.CombinedOutput(); ferr != nil {
		if strings.Contains(string(out), "couldn't find remote ref") {
			return "", "", &errBranchNotFound{Repo: repoName, Branch: featureBranch}
		}
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
	//    git will append enclosing function names to @@ hunk headers because
	//    WriteGitInfoAttributes has already wired up the built-in diff drivers
	//    (python, golang, cpp, …) via .git/info/attributes.

	// Always (re-)fetch the base branch so that origin/<baseBranch> is a real
	// remote-tracking ref with sufficient history for the three-dot diff below.
	// A conditional check is not enough: a shallow clone may have the ref but
	// lack the merge-base commit, causing `git diff origin/<base>...FETCH_HEAD`
	// to exit 128.  The fetch also handles base branches with slashes (e.g.
	// "tce/master") where the ref path can be ambiguous in some git versions.
	baseRemoteRef := fmt.Sprintf("refs/remotes/origin/%s", baseBranch)
	fetchBaseCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "--depth=50", "origin",
		fmt.Sprintf("refs/heads/%s:%s", baseBranch, baseRemoteRef))
	if fetchBaseOut, fetchBaseErr := fetchBaseCmd.CombinedOutput(); fetchBaseErr != nil {
		return "", baseBranch, fmt.Errorf("git fetch origin %s (base branch for diff): %w\n%s", baseBranch, fetchBaseErr, fetchBaseOut)
	}

	diffRef := fmt.Sprintf("origin/%s...FETCH_HEAD", baseBranch)
	diffCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "diff", diffRef)
	out, derr := diffCmd.Output()
	if derr != nil {
		// Exit code 1 from git diff means "non-empty diff" — that's fine.
		// Exit code 128 often means shallow clone lacks the merge-base commit;
		// deepen and retry once before giving up.
		exitErr, isExit := derr.(*exec.ExitError)
		if !isExit {
			return "", baseBranch, fmt.Errorf("git diff %s: %w", diffRef, derr)
		}
		if exitErr.ExitCode() == 128 {
			// Deepen both sides and retry.
			exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "--deepen=100", "origin").Run() //nolint:errcheck
			exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "--depth=100", "origin",
				fmt.Sprintf("refs/heads/%s:%s", baseBranch, baseRemoteRef)).Run() //nolint:errcheck
			diffCmd2 := exec.CommandContext(ctx, "git", "-C", repoDir, "diff", diffRef)
			out2, derr2 := diffCmd2.Output()
			if derr2 != nil {
				if exitErr2, ok2 := derr2.(*exec.ExitError); !ok2 || exitErr2.ExitCode() != 1 {
					return "", baseBranch, fmt.Errorf("git diff %s: %w", diffRef, derr2)
				}
			}
			out = out2
		} else if exitErr.ExitCode() != 1 {
			return "", baseBranch, fmt.Errorf("git diff %s: %w", diffRef, derr)
		}
	}

	diffStr := strings.TrimSpace(string(out))
	if diffStr == "" {
		// The branch may have already been merged into baseBranch.
		// Try to recover the diff from the merge commit on origin/<baseBranch>.
		recovered, mergeErr := recoverMergedBranchDiff(ctx, repoDir, baseBranch, featureBranch)
		if mergeErr == nil && recovered != "" {
			return recovered, baseBranch, nil
		}
		detail := ""
		if mergeErr != nil {
			detail = mergeErr.Error()
		}
		// Return errEmptyDiff so callers can skip this repo gracefully instead
		// of aborting the whole multi-repo submission.
		return "", baseBranch, &errEmptyDiff{
			Repo:       repoName,
			Branch:     featureBranch,
			BaseBranch: baseBranch,
			Detail:     detail,
		}
	}
	return diffStr, baseBranch, nil
}

// recoverMergedBranchDiff searches for a merge commit on origin/<baseBranch>
// that incorporated featureBranch, then returns the diff introduced by that commit.
// This handles the common case where a feature branch has already been merged.
func recoverMergedBranchDiff(ctx context.Context, repoDir, baseBranch, featureBranch string) (string, error) {
	// Deepen the base branch history so we can find the merge commit.
	fetchBase := exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "--depth=200", "origin", baseBranch)
	if out, err := fetchBase.CombinedOutput(); err != nil {
		return "", fmt.Errorf("fetch base branch: %w\n%s", err, out)
	}

	// List merge commits on origin/<baseBranch>, most recent first.
	// --merges restricts to commits with ≥2 parents (actual merges).
	logCmd := exec.CommandContext(ctx, "git", "-C", repoDir,
		"log", fmt.Sprintf("origin/%s", baseBranch),
		"--merges", "--format=%H %s", "--max-count=500",
	)
	logOut, err := logCmd.Output()
	if err != nil {
		return "", fmt.Errorf("git log --merges: %w", err)
	}

	// Strip refs/heads/ prefix for matching against commit subjects.
	shortBranch := featureBranch
	if strings.HasPrefix(shortBranch, "refs/heads/") {
		shortBranch = shortBranch[len("refs/heads/"):]
	}
	// Also prepare a shorter variant: last segment after the final slash
	// (e.g. "feature/fix-abc" → "fix-abc") for more permissive matching.
	branchTail := shortBranch
	if idx := strings.LastIndex(shortBranch, "/"); idx >= 0 {
		branchTail = shortBranch[idx+1:]
	}

	var mergeHash string
	for _, line := range strings.Split(strings.TrimSpace(string(logOut)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		hash, subject := parts[0], parts[1]
		if strings.Contains(subject, shortBranch) ||
			(len(branchTail) > 5 && strings.Contains(subject, branchTail)) {
			mergeHash = hash
			break
		}
	}

	if mergeHash == "" {
		return "", fmt.Errorf("no merge commit found for branch %q in last 500 merges on %s", shortBranch, baseBranch)
	}

	// Compute diff introduced by the merge commit: compare merge commit against its first parent.
	diffCmd := exec.CommandContext(ctx, "git", "-C", repoDir,
		"diff", mergeHash+"^1", mergeHash,
	)
	diffOut, err := diffCmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			return "", fmt.Errorf("git diff merge commit %s: %w", mergeHash, err)
		}
	}

	result := strings.TrimSpace(string(diffOut))
	if result == "" {
		return "", fmt.Errorf("merge commit %s has empty diff", mergeHash)
	}
	return result, nil
}

// isEmptyResult reports whether a completed TaskResult has no meaningful output.
// A result is considered empty when:
//   - call_chain is nil/null/[] OR contains only nodes with empty File fields
//     (LLM internal monologue nodes that slipped through)
//   - entry_points is nil/null/[]
func isEmptyResult(r *storage.TaskResult) bool {
	chainEmpty := len(r.CallChain) == 0 ||
		string(r.CallChain) == "null" ||
		string(r.CallChain) == "[]" ||
		callChainHasNoFiles(r.CallChain)
	epEmpty := len(r.EntryPoints) == 0 ||
		string(r.EntryPoints) == "null" ||
		string(r.EntryPoints) == "[]"
	return chainEmpty && epEmpty
}

// callChainHasNoFiles returns true when every node in the call_chain JSON array
// has an empty "File" field — which happens when the LLM writes its reasoning
// as a function name instead of emitting a real call node.
func callChainHasNoFiles(raw json.RawMessage) bool {
	var nodes []struct {
		File string `json:"File"`
	}
	if err := json.Unmarshal(raw, &nodes); err != nil || len(nodes) == 0 {
		return false
	}
	for _, n := range nodes {
		if strings.TrimSpace(n.File) != "" {
			return false
		}
	}
	return true
}

// buildEmptyResultWarnings inspects the stored inputDiff and returns a list of
// human-readable, actionable diagnostic messages explaining why no results were
// produced. Returns nil when the diff looks fine (no obvious issue detected).
func buildEmptyResultWarnings(inputDiff string) []string {
	if strings.TrimSpace(inputDiff) == "" {
		return []string{
			"input_diff 为空，无法提取变更函数。请提供 unified diff（git diff 输出格式）。",
		}
	}

	hunks := itool.ParseDiffHunks(inputDiff)
	if len(hunks) == 0 {
		return []string{
			"diff 中未解析到任何 hunk（@@ 块）。请确认提交的是标准 unified diff 格式（git diff 输出）。",
		}
	}

	funcs := itool.ParseDiffFunctions(inputDiff)
	if len(funcs) == 0 {
		return []string{
			"diff 包含有效 hunk，但未识别到任何变更函数名，导致分析无起点。" +
				"常见原因：@@ 行末尾缺少函数/方法名。" +
				"git 自动生成的 diff 会在 @@ 行末尾附加当前函数名，例如：" +
				"@@ -209,10 +209,10 @@ def get_data(self): 。" +
				"手写 diff 时请在 @@ 行末尾补充所在函数名。",
		}
	}

	return nil
}

// buildFailedWarnings generates human-readable diagnostic hints for a failed task.
// It inspects the stored error message and returns actionable guidance for the caller.
func buildFailedWarnings(errMsg string) []string {
	if errMsg == "" {
		return []string{"分析任务失败，原因未知。请重新提交任务重试。"}
	}

	lower := strings.ToLower(errMsg)

	// Rate-limit / quota errors (HTTP 429, "rate limit", "quota", "too many requests").
	if strings.Contains(lower, "429") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "quota") {
		return []string{
			"LLM API 请求频率超限（429 Too Many Requests）。" +
				"请等待约 1 分钟后重新提交任务。" +
				"如频繁出现此错误，请联系管理员提升 API 配额或降低并发分析数量。",
		}
	}

	// Timeout errors.
	if strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "context deadline") {
		return []string{
			"分析超时。diff 变更量可能过大，或 LLM 响应延迟较高。" +
				"建议拆分 diff 为更小的变更单元后重试，或稍后重新提交。",
		}
	}

	// Network / connection errors.
	if strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "no such host") ||
		strings.Contains(lower, "network") ||
		strings.Contains(lower, "dial") {
		return []string{
			"无法连接到 LLM API（网络错误）。请检查服务网络配置后重新提交任务。",
		}
	}

	// Authentication errors (401/403).
	if strings.Contains(lower, "401") ||
		strings.Contains(lower, "403") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "forbidden") ||
		strings.Contains(lower, "invalid api key") {
		return []string{
			"LLM API 认证失败（401/403）。请检查 API Key 配置是否正确，然后重新提交任务。",
		}
	}

	// Fallback: surface raw error with retry hint.
	return []string{
		fmt.Sprintf("分析任务失败：%s。请重新提交任务重试，若问题持续请联系管理员。", errMsg),
	}
}

// extractGhostWarnings parses the ghost_files sentinel line embedded in ImpactSummary
// and converts each hallucinated file path into a human-readable warning string.
//
// Format of the sentinel line (appended by runAnalysis):
//
//	<!-- ghost_files: ["cvm_api/api/Foo.py","cvm_api/entry/instance/Foo.py"] -->
func extractGhostWarnings(impactSummary string) []string {
	const prefix = "<!-- ghost_files: "
	const suffix = " -->"
	idx := strings.Index(impactSummary, prefix)
	if idx < 0 {
		return nil
	}
	start := idx + len(prefix)
	end := strings.Index(impactSummary[start:], suffix)
	if end < 0 {
		return nil
	}
	raw := impactSummary[start : start+end]

	var paths []string
	if err := json.Unmarshal([]byte(raw), &paths); err != nil || len(paths) == 0 {
		return nil
	}

	warns := make([]string, 0, len(paths))
	for _, p := range paths {
		warns = append(warns, fmt.Sprintf(
			"分析结果中检测到幻觉路径（文件不存在）：%s — 该节点已被过滤。"+
				"如需完整追踪，请确认函数命名风格（驼峰/蛇形）或通过 dispatch 机制调用。",
			p,
		))
	}
	return warns
}

// ---------------------------------------------------------------------------
// Nightly index rebuild (改进 2)
// ---------------------------------------------------------------------------

// cstLocation is Asia/Shanghai (UTC+8), used for nightly index scheduling.
var cstLocation = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		// Fallback to a fixed +8 offset if the timezone database is unavailable.
		loc = time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// scheduleNightlyIndex waits until the next 04:00 CST then rebuilds the symbol
// index for all configured repositories. It loops indefinitely — once per night.
// The goroutine exits when ctx is cancelled.
func (s *apiServer) scheduleNightlyIndex(ctx context.Context) {
	log := logger.Must("production")
	for {
		next := nextRunAt(time.Now().In(cstLocation), 4, 0)
		delay := time.Until(next)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		// Fire.
		log.Sugar().Infow("nightly_index_start", "scheduled_for", next.Format(time.RFC3339))
		s.runNightlyIndex(ctx)
		log.Sugar().Infow("nightly_index_done")
	}
}

// nextRunAt returns the next wall-clock time when hour:minute occurs in the
// given location. If now is already past that time today, it returns tomorrow.
func nextRunAt(now time.Time, hour, minute int) time.Time {
	candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !candidate.After(now) {
		candidate = candidate.Add(24 * time.Hour)
	}
	return candidate
}

// runNightlyIndex rebuilds the full symbol index for every configured repository.
// Language is auto-detected: repositories that contain .go files use GoIndexer;
// repositories that contain only .py files use PythonIndexer.
// Each repo's index is rebuilt serially to avoid overloading the NFS workspace.
func (s *apiServer) runNightlyIndex(ctx context.Context) {
	log := logger.Must("production")

	// Respect the same FIFO semaphore as runAnalysis so that a nightly index run
	// never bypasses the server's concurrency limit and races with user tasks.
	s.queueCounter.Add(1)
	s.semaphore <- struct{}{}
	s.queueCounter.Add(-1)
	defer func() { <-s.semaphore }()

	store := index.NewStore(s.pool)

	for _, r := range s.cfg.Workspace.Repos {
		if ctx.Err() != nil {
			return
		}

		repoPath := filepath.Join(s.cfg.Workspace.Dir, r.Name)

		// Determine current HEAD commit hash.
		commitHash := ""
		if out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "HEAD").Output(); err == nil {
			commitHash = strings.TrimSpace(string(out))
		}

		// Detect language.
		lang := detectRepoLanguage(repoPath)
		if lang == "unknown" {
			log.Sugar().Infow("nightly_index_skip", "repo", r.Name, "reason", "unknown language")
			continue
		}

		start := time.Now()
		log.Sugar().Infow("nightly_index_repo_start", "repo", r.Name, "lang", lang, "commit", commitHash)

		var result *index.IndexResult
		var err error

		switch lang {
		case "go":
			result, err = index.NewGoIndexer(r.Name, repoPath, commitHash).Index()
		case "python":
			result, err = index.NewPythonIndexer(r.Name, repoPath, commitHash).Index()
		}

		if err != nil {
			log.Sugar().Warnw("nightly_index_repo_error", "repo", r.Name, "err", err)
			continue
		}

		// Atomically replace the existing index: delete then save.
		if delErr := store.DeleteByRepo(ctx, r.Name); delErr != nil {
			log.Sugar().Warnw("nightly_index_delete_error", "repo", r.Name, "err", delErr)
			continue
		}
		if saveErr := store.SaveNodes(ctx, result.Nodes); saveErr != nil {
			log.Sugar().Warnw("nightly_index_save_nodes_error", "repo", r.Name, "err", saveErr)
			continue
		}
		if saveErr := store.SaveEdges(ctx, result.Edges); saveErr != nil {
			log.Sugar().Warnw("nightly_index_save_edges_error", "repo", r.Name, "err", saveErr)
			// Non-fatal: nodes are saved; edges are best-effort.
		}

		durationMs := int(time.Since(start).Milliseconds())
		meta := index.IndexMetadata{
			Repo:         r.Name,
			CommitHash:   commitHash,
			IndexedAt:    time.Now(),
			TotalFiles:   result.Files,
			TotalSymbols: len(result.Nodes),
			TotalEdges:   len(result.Edges),
			Language:     lang,
			DurationMs:   durationMs,
		}
		if metaErr := store.SaveMetadata(ctx, meta); metaErr != nil {
			log.Sugar().Warnw("nightly_index_meta_error", "repo", r.Name, "err", metaErr)
		}

		log.Sugar().Infow("nightly_index_repo_done",
			"repo", r.Name,
			"symbols", len(result.Nodes),
			"edges", len(result.Edges),
			"files", result.Files,
			"duration_ms", durationMs,
		)
	}
}

// detectRepoLanguage returns "go", "python", or "unknown" by probing the repo
// directory for characteristic file extensions. Go is checked first because
// many repos contain both .go and .py (scripts, tools) but their primary
// language is Go.
func detectRepoLanguage(repoPath string) string {
	// Check for go.mod as the canonical Go indicator.
	if _, err := os.Stat(filepath.Join(repoPath, "go.mod")); err == nil {
		return "go"
	}
	// Check for setup.py / pyproject.toml / requirements.txt as Python indicators.
	for _, marker := range []string{"setup.py", "pyproject.toml", "requirements.txt"} {
		if _, err := os.Stat(filepath.Join(repoPath, marker)); err == nil {
			return "python"
		}
	}
	return "unknown"
}
