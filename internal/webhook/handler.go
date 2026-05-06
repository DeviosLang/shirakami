// Package webhook handles incoming CI/CD webhook events from GitLab and GitHub.
//
// Supported events:
//   - GitLab Merge Request Hook (X-Gitlab-Event: Merge Request Hook)
//   - GitHub Pull Request event (X-GitHub-Event: pull_request)
//
// For each opened/updated MR/PR the handler:
//  1. Verifies the secret token (GitLab plain-text header; GitHub HMAC-SHA256).
//  2. Extracts the diff from the event payload (object_attributes.last_commit.url
//     for GitLab, pull.diff_url for GitHub).
//  3. Submits an analysis task via the task store.
//  4. Optionally posts a comment back to the MR/PR with the task ID / result URL.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Taskmaster interface — satisfied by *storage.Store without importing it.
// ---------------------------------------------------------------------------

// TaskCreator is the minimal interface the webhook handler needs from the
// storage layer.  It is satisfied by *storage.Store without creating an import
// cycle.
type TaskCreator interface {
	CreateTask(ctx context.Context, inputType, inputDiff, inputDesc, cacheKey string) (TaskRecord, error)
}

// TaskRecord is the minimal information returned after creating a task.
type TaskRecord struct {
	ID string
}

// AnalysisLauncher is called after task creation to start the background
// analysis.  The server passes its own goroutine launcher here so the webhook
// package stays independent of the agent package.
type AnalysisLauncher func(taskID, inputDiff, inputDesc, cacheKey string)

// Commenter posts a comment on the originating MR/PR.
// Implementations handle GitLab and GitHub separately.
// Passing nil disables commenting.
type Commenter interface {
	PostComment(ctx context.Context, event *ParsedEvent, taskID string) error
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// Config holds runtime options for the webhook handler.
type Config struct {
	// Secret is the shared secret for signature verification.
	// Empty = verification disabled (not recommended for production).
	Secret string

	// Commenter posts task IDs / analysis summaries back to the MR/PR.
	// nil = silent (no comments posted).
	Commenter Commenter

	// Launch starts the background analysis after task creation.
	Launch AnalysisLauncher
}

// Handler implements http.Handler for POST /api/v1/webhook.
type Handler struct {
	cfg    Config
	store  TaskCreator
}

// New creates a new webhook Handler.
func New(store TaskCreator, cfg Config) *Handler {
	return &Handler{cfg: cfg, store: store}
}

// ServeHTTP dispatches to the appropriate provider handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read and buffer body so we can both verify and parse it.
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MB limit
	if err != nil {
		jsonError(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Detect provider.
	var event *ParsedEvent
	switch {
	case r.Header.Get("X-Gitlab-Event") != "":
		if err := h.verifyGitLab(r, body); err != nil {
			jsonError(w, "signature verification failed: "+err.Error(), http.StatusUnauthorized)
			return
		}
		event, err = parseGitLabEvent(r.Header.Get("X-Gitlab-Event"), body)

	case r.Header.Get("X-GitHub-Event") != "":
		if err := h.verifyGitHub(r, body); err != nil {
			jsonError(w, "signature verification failed: "+err.Error(), http.StatusUnauthorized)
			return
		}
		event, err = parseGitHubEvent(r.Header.Get("X-GitHub-Event"), body)

	default:
		jsonError(w, "unrecognised webhook provider (missing X-Gitlab-Event or X-GitHub-Event header)", http.StatusBadRequest)
		return
	}

	if err != nil {
		jsonError(w, "parse event: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	// Ignore non-actionable events (e.g. MR closed, comment events, etc.).
	if event == nil || !event.Actionable {
		jsonOK(w, map[string]string{"status": "ignored"})
		return
	}

	// Submit analysis task.
	ctx := r.Context()
	cacheKey := cacheKeyFor(event)
	rec, err := h.store.CreateTask(ctx, "diff", event.Diff, event.Description, cacheKey)
	if err != nil {
		jsonError(w, "create task: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Launch background analysis (non-blocking).
	if h.cfg.Launch != nil {
		go h.cfg.Launch(rec.ID, event.Diff, event.Description, cacheKey)
	}

	// Post comment (non-blocking, best-effort).
	if h.cfg.Commenter != nil {
		go func() {
			postCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = h.cfg.Commenter.PostComment(postCtx, event, rec.ID)
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"status":  "accepted",
		"task_id": rec.ID,
	})
}

// ---------------------------------------------------------------------------
// Signature verification
// ---------------------------------------------------------------------------

// verifyGitLab checks the X-Gitlab-Token header against the configured secret.
// GitLab uses a plain-text comparison (not HMAC).
func (h *Handler) verifyGitLab(r *http.Request, _ []byte) error {
	if h.cfg.Secret == "" {
		return nil // verification disabled
	}
	token := r.Header.Get("X-Gitlab-Token")
	if token != h.cfg.Secret {
		return fmt.Errorf("X-Gitlab-Token mismatch")
	}
	return nil
}

// verifyGitHub checks the X-Hub-Signature-256 header using HMAC-SHA256.
func (h *Handler) verifyGitHub(r *http.Request, body []byte) error {
	if h.cfg.Secret == "" {
		return nil // verification disabled
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		return fmt.Errorf("missing X-Hub-Signature-256 header")
	}
	// Expected format: "sha256=<hex>"
	if !strings.HasPrefix(sig, "sha256=") {
		return fmt.Errorf("unexpected signature format: %s", sig)
	}
	expected, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	if err != nil {
		return fmt.Errorf("decode signature hex: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(h.cfg.Secret))
	mac.Write(body)
	computed := mac.Sum(nil)
	if !hmac.Equal(expected, computed) {
		return fmt.Errorf("HMAC mismatch")
	}
	return nil
}

// ---------------------------------------------------------------------------
// ParsedEvent — normalised representation from either provider
// ---------------------------------------------------------------------------

// ParsedEvent is the normalised event extracted from a GitLab or GitHub payload.
type ParsedEvent struct {
	Provider    string // "gitlab" | "github"
	Action      string // "open" | "update" | "close" | ...
	Actionable  bool   // true if we should trigger analysis
	RepoName    string
	MRPRURL     string // URL of the MR/PR
	MRPRID      int    // numeric ID of the MR/PR
	Diff        string // raw unified diff (may be empty if fetched async)
	Description string // title + description for context

	// Provider-specific data needed for posting comments.
	GitLab *GitLabEventMeta
	GitHub *GitHubEventMeta
}

// GitLabEventMeta holds GitLab-specific fields needed for posting MR comments.
type GitLabEventMeta struct {
	ProjectID   int
	MRIID       int    // iid (internal project ID of the MR)
	ProjectPath string // e.g. "group/project"
}

// GitHubEventMeta holds GitHub-specific fields needed for posting PR comments.
type GitHubEventMeta struct {
	Owner  string
	Repo   string
	PRNumber int
}

// ---------------------------------------------------------------------------
// GitLab event parsing
// ---------------------------------------------------------------------------

// gitlabMRPayload is the subset of the GitLab Merge Request Hook payload we
// need.  Full schema: https://docs.gitlab.com/ee/user/project/integrations/webhook_events.html#merge-request-events
type gitlabMRPayload struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		IID         int    `json:"iid"`
		URL         string `json:"url"`
		State       string `json:"state"`
		Action      string `json:"action"`
		Title       string `json:"title"`
		Description string `json:"description"`
		LastCommit  struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"last_commit"`
		DiffRefs struct {
			BaseSHA  string `json:"base_sha"`
			HeadSHA  string `json:"head_sha"`
			StartSHA string `json:"start_sha"`
		} `json:"diff_refs"`
	} `json:"object_attributes"`
	Project struct {
		ID                int    `json:"id"`
		Name              string `json:"name"`
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
		HTTPURLToRepo     string `json:"http_url_to_repo"`
	} `json:"project"`
}

func parseGitLabEvent(eventType string, body []byte) (*ParsedEvent, error) {
	if eventType != "Merge Request Hook" {
		// Only Merge Request Hook is handled; silently ignore others.
		return nil, nil
	}

	var payload gitlabMRPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal GitLab MR payload: %w", err)
	}
	if payload.ObjectKind != "merge_request" {
		return nil, nil
	}

	oa := payload.ObjectAttributes
	action := oa.Action // "open", "update", "close", "reopen", "merge", etc.

	// We act on open, update (push to source branch), and reopen.
	actionable := action == "open" || action == "update" || action == "reopen"

	event := &ParsedEvent{
		Provider:   "gitlab",
		Action:     action,
		Actionable: actionable,
		RepoName:   payload.Project.PathWithNamespace,
		MRPRURL:    oa.URL,
		MRPRID:     oa.IID,
		Description: fmt.Sprintf("MR !%d: %s\n\n%s", oa.IID, oa.Title, oa.Description),
		GitLab: &GitLabEventMeta{
			ProjectID:   payload.Project.ID,
			MRIID:       oa.IID,
			ProjectPath: payload.Project.PathWithNamespace,
		},
	}

	// Diff is fetched lazily by the caller; here we build a placeholder that
	// includes the base/head SHAs so it can be retrieved if needed.
	if oa.DiffRefs.BaseSHA != "" && oa.DiffRefs.HeadSHA != "" {
		event.Diff = fmt.Sprintf("# GitLab MR diff: base=%s head=%s repo=%s",
			oa.DiffRefs.BaseSHA, oa.DiffRefs.HeadSHA, payload.Project.HTTPURLToRepo)
	}

	return event, nil
}

// ---------------------------------------------------------------------------
// GitHub event parsing
// ---------------------------------------------------------------------------

// githubPRPayload is the subset of the GitHub pull_request event we need.
// Full schema: https://docs.github.com/en/developers/webhooks-and-events/webhooks/webhook-events-and-payloads#pull_request
type githubPRPayload struct {
	Action string `json:"action"`
	Number int    `json:"number"`
	PullRequest struct {
		HTMLURL string `json:"html_url"`
		DiffURL string `json:"diff_url"`
		State   string `json:"state"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		Head    struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
}

func parseGitHubEvent(eventType string, body []byte) (*ParsedEvent, error) {
	if eventType != "pull_request" {
		return nil, nil
	}

	var payload githubPRPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal GitHub PR payload: %w", err)
	}

	action := payload.Action // "opened", "synchronize", "reopened", "closed", etc.
	actionable := action == "opened" || action == "synchronize" || action == "reopened"

	pr := payload.PullRequest
	parts := strings.SplitN(payload.Repository.FullName, "/", 2)
	owner, repo := "", payload.Repository.FullName
	if len(parts) == 2 {
		owner, repo = parts[0], parts[1]
	}

	event := &ParsedEvent{
		Provider:   "github",
		Action:     action,
		Actionable: actionable,
		RepoName:   payload.Repository.FullName,
		MRPRURL:    pr.HTMLURL,
		MRPRID:     payload.Number,
		Description: fmt.Sprintf("PR #%d: %s\n\n%s", payload.Number, pr.Title, pr.Body),
		Diff:       pr.DiffURL, // caller fetches actual diff from this URL
		GitHub: &GitHubEventMeta{
			Owner:    owner,
			Repo:     repo,
			PRNumber: payload.Number,
		},
	}

	return event, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func cacheKeyFor(e *ParsedEvent) string {
	h := sha256.Sum256([]byte(e.Provider + "|" + e.MRPRURL + "|" + e.Diff))
	return hex.EncodeToString(h[:])
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
// Commenter implementations
// ---------------------------------------------------------------------------

// GitLabCommenter posts a note on a GitLab MR.
type GitLabCommenter struct {
	Token      string
	BaseURL    string // e.g. "https://gitlab.com" (no trailing slash)
	HTTPClient *http.Client
}

// PostComment posts a single note on the GitLab MR.
func (c *GitLabCommenter) PostComment(ctx context.Context, event *ParsedEvent, taskID string) error {
	if event.GitLab == nil {
		return nil
	}
	gl := event.GitLab
	url := fmt.Sprintf("%s/api/v4/projects/%d/merge_requests/%d/notes",
		strings.TrimRight(c.BaseURL, "/"), gl.ProjectID, gl.MRIID)

	body := map[string]string{
		"body": fmt.Sprintf("🤖 **Shirakami analysis started** — task `%s`\n\n"+
			"Results will be available at `/api/v1/tasks/%s` once the analysis completes.",
			taskID, taskID),
	}
	bodyJSON, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("PRIVATE-TOKEN", c.Token)

	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("post GitLab note: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitLab API %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// GitHubCommenter posts a review comment on a GitHub PR.
type GitHubCommenter struct {
	Token      string
	HTTPClient *http.Client
}

// PostComment posts a single issue comment on the GitHub PR.
func (c *GitHubCommenter) PostComment(ctx context.Context, event *ParsedEvent, taskID string) error {
	if event.GitHub == nil {
		return nil
	}
	gh := event.GitHub
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments",
		gh.Owner, gh.Repo, gh.PRNumber)

	body := map[string]string{
		"body": fmt.Sprintf("🤖 **Shirakami analysis started** — task `%s`\n\n"+
			"Results will be available at `/api/v1/tasks/%s` once the analysis completes.",
			taskID, taskID),
	}
	bodyJSON, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("post GitHub comment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}
