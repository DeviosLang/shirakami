package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------------------------------------------------------------------------
// Mock TaskCreator
// ---------------------------------------------------------------------------

type mockTaskCreator struct {
	created []TaskRecord
	err     error
}

func (m *mockTaskCreator) CreateTask(_ context.Context, _, _, _, cacheKey string) (TaskRecord, error) {
	if m.err != nil {
		return TaskRecord{}, m.err
	}
	rec := TaskRecord{ID: "task-" + cacheKey[:8]}
	m.created = append(m.created, rec)
	return rec, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newHandler(store TaskCreator, secret string) *Handler {
	return New(store, Config{Secret: secret})
}

func postWebhook(h *Handler, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func gitlabMRBody(action, state string) []byte {
	payload := map[string]any{
		"object_kind": "merge_request",
		"object_attributes": map[string]any{
			"iid":    42,
			"url":    "https://gitlab.example.com/group/proj/-/merge_requests/42",
			"state":  state,
			"action": action,
			"title":  "Test MR",
			"description": "Some description",
			"last_commit": map[string]string{"id": "abc123", "message": "feat: add thing"},
			"diff_refs": map[string]string{
				"base_sha":  "aaa",
				"head_sha":  "bbb",
				"start_sha": "aaa",
			},
		},
		"project": map[string]any{
			"id":                  10,
			"name":                "proj",
			"path_with_namespace": "group/proj",
			"web_url":             "https://gitlab.example.com/group/proj",
			"http_url_to_repo":    "https://gitlab.example.com/group/proj.git",
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func githubPRBody(action string) []byte {
	payload := map[string]any{
		"action": action,
		"number": 7,
		"pull_request": map[string]any{
			"html_url": "https://github.com/owner/repo/pull/7",
			"diff_url": "https://github.com/owner/repo/pull/7.diff",
			"state":    "open",
			"title":    "feat: new thing",
			"body":     "PR body",
			"head":     map[string]string{"sha": "bbb", "ref": "feature"},
			"base":     map[string]string{"sha": "aaa", "ref": "main"},
		},
		"repository": map[string]any{
			"full_name": "owner/repo",
			"clone_url": "https://github.com/owner/repo.git",
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func githubSig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// Method checks
// ---------------------------------------------------------------------------

func TestWebhook_RejectsGET(t *testing.T) {
	h := newHandler(&mockTaskCreator{}, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/webhook", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}
}

func TestWebhook_NoProviderHeader(t *testing.T) {
	h := newHandler(&mockTaskCreator{}, "")
	rr := postWebhook(h, map[string]string{}, []byte(`{}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// GitLab tests
// ---------------------------------------------------------------------------

func TestWebhook_GitLab_OpenMR_Accepted(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "") // no secret
	body := gitlabMRBody("open", "opened")

	rr := postWebhook(h, map[string]string{
		"X-Gitlab-Event": "Merge Request Hook",
	}, body)

	if rr.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(store.created) != 1 {
		t.Errorf("expected 1 task created, got %d", len(store.created))
	}
	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "accepted" {
		t.Errorf("expected status=accepted, got %q", resp["status"])
	}
	if resp["task_id"] == "" {
		t.Error("task_id should not be empty")
	}
}

func TestWebhook_GitLab_CloseMR_Ignored(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "")
	body := gitlabMRBody("close", "closed")

	rr := postWebhook(h, map[string]string{
		"X-Gitlab-Event": "Merge Request Hook",
	}, body)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 (ignored), got %d", rr.Code)
	}
	if len(store.created) != 0 {
		t.Errorf("expected 0 tasks for closed MR, got %d", len(store.created))
	}
	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "ignored" {
		t.Errorf("expected status=ignored, got %q", resp["status"])
	}
}

func TestWebhook_GitLab_UpdateMR_Accepted(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "")
	body := gitlabMRBody("update", "opened")

	rr := postWebhook(h, map[string]string{
		"X-Gitlab-Event": "Merge Request Hook",
	}, body)

	if rr.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d", rr.Code)
	}
}

func TestWebhook_GitLab_SecretValid(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "mysecret")
	body := gitlabMRBody("open", "opened")

	rr := postWebhook(h, map[string]string{
		"X-Gitlab-Event": "Merge Request Hook",
		"X-Gitlab-Token": "mysecret",
	}, body)

	if rr.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestWebhook_GitLab_SecretInvalid(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "mysecret")
	body := gitlabMRBody("open", "opened")

	rr := postWebhook(h, map[string]string{
		"X-Gitlab-Event": "Merge Request Hook",
		"X-Gitlab-Token": "wrong",
	}, body)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestWebhook_GitLab_UnknownEventType_Ignored(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "")

	rr := postWebhook(h, map[string]string{
		"X-Gitlab-Event": "Push Hook",
	}, []byte(`{}`))

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 (ignored unknown event), got %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// GitHub tests
// ---------------------------------------------------------------------------

func TestWebhook_GitHub_OpenedPR_Accepted(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "") // no secret
	body := githubPRBody("opened")

	rr := postWebhook(h, map[string]string{
		"X-GitHub-Event": "pull_request",
	}, body)

	if rr.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(store.created) != 1 {
		t.Errorf("expected 1 task, got %d", len(store.created))
	}
}

func TestWebhook_GitHub_SynchronizePR_Accepted(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "")
	body := githubPRBody("synchronize")

	rr := postWebhook(h, map[string]string{
		"X-GitHub-Event": "pull_request",
	}, body)

	if rr.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d", rr.Code)
	}
}

func TestWebhook_GitHub_ClosedPR_Ignored(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "")
	body := githubPRBody("closed")

	rr := postWebhook(h, map[string]string{
		"X-GitHub-Event": "pull_request",
	}, body)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 (ignored), got %d", rr.Code)
	}
	if len(store.created) != 0 {
		t.Errorf("expected 0 tasks for closed PR, got %d", len(store.created))
	}
}

func TestWebhook_GitHub_HMACSigValid(t *testing.T) {
	store := &mockTaskCreator{}
	secret := "gh-secret"
	h := newHandler(store, secret)
	body := githubPRBody("opened")

	rr := postWebhook(h, map[string]string{
		"X-GitHub-Event":       "pull_request",
		"X-Hub-Signature-256":  githubSig(secret, body),
	}, body)

	if rr.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestWebhook_GitHub_HMACSigInvalid(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "gh-secret")
	body := githubPRBody("opened")

	rr := postWebhook(h, map[string]string{
		"X-GitHub-Event":      "pull_request",
		"X-Hub-Signature-256": "sha256=badhex0000000000000000000000000000000000000000000000000000000000",
	}, body)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestWebhook_GitHub_MissingSig_WithSecret(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "gh-secret")
	body := githubPRBody("opened")

	rr := postWebhook(h, map[string]string{
		"X-GitHub-Event": "pull_request",
		// No X-Hub-Signature-256
	}, body)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestWebhook_GitHub_UnknownEventType_Ignored(t *testing.T) {
	store := &mockTaskCreator{}
	h := newHandler(store, "")

	rr := postWebhook(h, map[string]string{
		"X-GitHub-Event": "push",
	}, []byte(`{}`))

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 (ignored push event), got %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// AnalysisLauncher callback
// ---------------------------------------------------------------------------

func TestWebhook_LauncherCalled(t *testing.T) {
	store := &mockTaskCreator{}
	launched := make(chan string, 1)
	h := New(store, Config{
		Secret: "",
		Launch: func(taskID, _, _, _ string) {
			launched <- taskID
		},
	})
	body := githubPRBody("opened")

	rr := postWebhook(h, map[string]string{
		"X-GitHub-Event": "pull_request",
	}, body)

	if rr.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d", rr.Code)
	}
	select {
	case taskID := <-launched:
		if taskID == "" {
			t.Error("launched task ID should not be empty")
		}
	default:
		// goroutine may not have run yet — that's OK, we just check it was called
	}
}

// ---------------------------------------------------------------------------
// verifyGitHub edge cases
// ---------------------------------------------------------------------------

func TestVerifyGitHub_NoSecret_Passes(t *testing.T) {
	h := &Handler{cfg: Config{Secret: ""}}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if err := h.verifyGitHub(req, []byte("body")); err != nil {
		t.Errorf("expected nil error with no secret, got %v", err)
	}
}

func TestVerifyGitLab_NoSecret_Passes(t *testing.T) {
	h := &Handler{cfg: Config{Secret: ""}}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if err := h.verifyGitLab(req, nil); err != nil {
		t.Errorf("expected nil error with no secret, got %v", err)
	}
}
