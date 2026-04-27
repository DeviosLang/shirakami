package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"testing"

	"github.com/DeviosLang/shirakami/internal/cache"
	"github.com/DeviosLang/shirakami/internal/config"
	"github.com/DeviosLang/shirakami/internal/feedback"
	"github.com/DeviosLang/shirakami/internal/storage"
)

// testAPIServer is a test-only HTTP server that mirrors cmd/server but uses
// injected dependencies instead of a real LLM.
type testAPIServer struct {
	store *storage.Store
	pool  *pgxpool.Pool
	cache *cache.Cache
	cfg   *config.Config
}

func newTestAPIServer(t *testing.T, cfg *config.Config, store *storage.Store, pool *pgxpool.Pool, rdb *redis.Client) http.Handler {
	t.Helper()
	srv := &testAPIServer{
		store: store,
		pool:  pool,
		cache: cache.New(rdb),
		cfg:   cfg,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/metrics", feedback.Handler())
	mux.HandleFunc("/api/v1/tasks", srv.handleTasks)
	mux.HandleFunc("/api/v1/tasks/", srv.handleTaskByID)
	return mux
}

type taskResponse struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	InputType   string     `json:"input_type"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func (s *testAPIServer) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.submitTask(w, r)
	case http.MethodGet:
		s.listTasks(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *testAPIServer) handleTaskByID(w http.ResponseWriter, r *http.Request) {
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

type submitRequest struct {
	InputType string `json:"input_type"`
	InputDiff string `json:"input_diff"`
	InputDesc string `json:"input_desc"`
}

func (s *testAPIServer) submitTask(w http.ResponseWriter, r *http.Request) {
	var req submitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	inputType := storage.InputType(req.InputType)
	if inputType == "" {
		inputType = storage.InputTypeDiff
	}

	cacheKey := cache.CacheKey(req.InputDiff+req.InputDesc, []string{s.cfg.Workspace.Dir})
	task, err := s.store.CreateTask(r.Context(), inputType, req.InputDiff, req.InputDesc, cacheKey)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(taskResponse{ //nolint:errcheck
		ID:        task.ID,
		Status:    string(task.Status),
		InputType: string(task.InputType),
		CreatedAt: task.CreatedAt,
	})
}

func (s *testAPIServer) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ListTasks(r.Context(), 20)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	responses := make([]taskResponse, 0, len(tasks))
	for _, t := range tasks {
		responses = append(responses, taskResponse{
			ID:          t.ID,
			Status:      string(t.Status),
			InputType:   string(t.InputType),
			CreatedAt:   t.CreatedAt,
			CompletedAt: t.CompletedAt,
		})
	}
	jsonOK(w, responses)
}

func (s *testAPIServer) getTask(w http.ResponseWriter, r *http.Request, id string) {
	task, err := s.store.GetTask(r.Context(), id)
	if err != nil {
		if err == storage.ErrNotFound {
			jsonError(w, "not found", http.StatusNotFound)
		} else {
			jsonError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	jsonOK(w, taskResponse{
		ID:          task.ID,
		Status:      string(task.Status),
		InputType:   string(task.InputType),
		CreatedAt:   task.CreatedAt,
		CompletedAt: task.CompletedAt,
	})
}

type feedbackRequest struct {
	Type    string `json:"type"`
	Comment string `json:"comment"`
}

func (s *testAPIServer) submitFeedback(w http.ResponseWriter, r *http.Request, taskID string) {
	var req feedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	valid := map[string]bool{
		"false_positive": true,
		"false_negative": true,
		"correct":        true,
	}
	if !valid[req.Type] {
		jsonError(w, "invalid type", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
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
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"status": "ok"})
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

// Ensure all background goroutines have time to finish.
func waitForCompletion(ctx context.Context, store *storage.Store, taskID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		if task.Status == storage.TaskStatusCompleted || task.Status == storage.TaskStatusFailed {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for task %s to complete", taskID)
}
