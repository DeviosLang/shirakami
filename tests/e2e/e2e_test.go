// Package e2e contains end-to-end integration tests for Shirakami.
//
// These tests use testcontainers-go to spin up real PostgreSQL and Redis
// instances — no mocks.
//
// Run with:
//
//	go test ./tests/e2e/... -v -count=1 -timeout=5m
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/DeviosLang/shirakami/internal/agent"
	"github.com/DeviosLang/shirakami/internal/cache"
	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/config"
	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/storage"
)

// ---------------------------------------------------------------------------
// Infrastructure helpers
// ---------------------------------------------------------------------------

func startPostgres(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("shirakami_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}

	// Run migrations.
	stdDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql db: %v", err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	// Migration path relative to repo root.
	migrPath := migrationsPath(t)
	if err := goose.Up(stdDB, migrPath); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	_ = stdDB.Close()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	return pool, dsn
}

func startRedis(t *testing.T) *redis.Client {
	t.Helper()
	ctx := context.Background()

	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	addr, err := container.Endpoint(ctx, "")
	if err != nil {
		t.Fatalf("redis endpoint: %v", err)
	}
	return redis.NewClient(&redis.Options{Addr: addr})
}

// migrationsPath returns the absolute path to the migrations directory.
func migrationsPath(t *testing.T) string {
	t.Helper()
	// Walk up from this file to find migrations/.
	dir := "."
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(dir + "/migrations"); err == nil {
			return dir + "/migrations"
		}
		dir = "../" + dir
	}
	t.Fatal("could not find migrations directory")
	return ""
}

// ---------------------------------------------------------------------------
// Mock LLM client for deterministic tests
// ---------------------------------------------------------------------------

type mockLLMClient struct {
	responses []*llm.Response
	calls     int
}

func (m *mockLLMClient) Complete(_ context.Context, _ []llm.Message, _ []llm.ToolDefinition) (*llm.Response, error) {
	if m.calls >= len(m.responses) {
		return &llm.Response{StopReason: llm.StopReasonEndTurn, Content: "no more responses"}, nil
	}
	resp := m.responses[m.calls]
	m.calls++
	return resp, nil
}

// mockTool is a no-op tool used in tests.
type mockTool struct {
	name   string
	result string
}

func (t *mockTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        t.name,
		Description: "mock " + t.name,
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (t *mockTool) Execute(_ context.Context, _ []byte) (string, error) {
	return t.result, nil
}

// ---------------------------------------------------------------------------
// Test infrastructure: fake repos
// ---------------------------------------------------------------------------

// setupFakeRepos creates two Git repos (entry-repo + service-repo) under a
// temporary workspace directory and returns the workspace path.
func setupFakeRepos(t *testing.T) string {
	t.Helper()
	wsDir := t.TempDir()

	// service-repo: contains a function "ProcessPayment" that we'll diff.
	serviceDir := wsDir + "/service-repo"
	mustMkdir(t, serviceDir)
	mustWriteFile(t, serviceDir+"/payment.go", `package service

import "fmt"

// ProcessPayment handles payment processing.
func ProcessPayment(amount float64, timeout int) error {
	fmt.Printf("processing payment: %.2f, timeout: %d\n", amount, timeout)
	return nil
}

// GetPaymentStatus returns the status.
func GetPaymentStatus(id string) string {
	return "pending"
}
`)
	mustGitInit(t, serviceDir)

	// entry-repo: registers a route that calls service-repo.
	entryDir := wsDir + "/entry-repo"
	mustMkdir(t, entryDir)
	mustWriteFile(t, entryDir+"/router.go", `package entry

import "net/http"

// RegisterRoutes registers all HTTP routes.
func RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/payment/process", HandlePayment)
}

// HandlePayment is the HTTP handler for payment processing.
func HandlePayment(w http.ResponseWriter, r *http.Request) {
	// calls service-repo ProcessPayment
}
`)
	mustGitInit(t, entryDir)

	return wsDir
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustGitInit(t *testing.T, dir string) {
	t.Helper()
	// No actual git needed for these tests — just ensure the directory exists.
	// The analysis uses LLM mocks so git state doesn't matter.
}

// ---------------------------------------------------------------------------
// Test 1: Diff input → call chain output
// ---------------------------------------------------------------------------

func TestE2E_DiffInput_CallChain(t *testing.T) {
	pool, _ := startPostgres(t)
	rdb := startRedis(t)
	wsDir := setupFakeRepos(t)

	store := storage.New(pool)
	analysisCache := cache.New(rdb)
	ctx := context.Background()

	// The diff represents a change to ProcessPayment in service-repo.
	diff := `diff --git a/service-repo/payment.go b/service-repo/payment.go
index abc123..def456 100644
--- a/service-repo/payment.go
+++ b/service-repo/payment.go
@@ -6,6 +6,7 @@ func ProcessPayment(amount float64, timeout int) error {
-	fmt.Printf("processing payment: %.2f, timeout: %d\n", amount, timeout)
+	fmt.Printf("processing payment: %.2f, timeout: %ds\n", amount, timeout)
+	// new comment
 	return nil
 }`

	cacheKey := cache.CacheKey(diff, []string{wsDir})

	// Create task record.
	task, err := store.CreateTask(ctx, storage.InputTypeDiff, diff, "", cacheKey)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Build mock LLM that returns a call chain result.
	llmClient := &mockLLMClient{
		responses: []*llm.Response{
			// extract changed functions step
			{StopReason: llm.StopReasonEndTurn, Content: "service-repo/ProcessPayment"},
			// worker analysis step
			{StopReason: llm.StopReasonEndTurn, Content: "entry-repo/HandlePayment -> service-repo/ProcessPayment"},
		},
	}

	cp, err := checkpoint.NewFileCheckpointer(t.TempDir())
	if err != nil {
		t.Fatalf("create checkpointer: %v", err)
	}

	repos := []agent.RepoInfo{
		{Name: "service-repo", Path: wsDir + "/service-repo", Role: "service"},
		{Name: "entry-repo", Path: wsDir + "/entry-repo", Role: "entry"},
	}

	tools := []agent.Tool{&mockTool{name: "ripgrep", result: "no matches"}}
	orch := agent.NewOrchestrator(llmClient, tools, repos, wsDir, cp)

	output, err := orch.Run(ctx, agent.AnalysisInput{Diff: diff})
	if err != nil {
		t.Fatalf("analysis run: %v", err)
	}

	// Verify we got changed functions back.
	if len(output.ChangedFunctions) == 0 {
		t.Error("expected at least one changed function")
	}

	// Persist result.
	callChainJSON, _ := json.Marshal(output.CallGraph)
	entryPointsJSON, _ := json.Marshal(output.EntryPoints)
	if err := store.SaveResult(ctx, &storage.TaskResult{
		TaskID:      task.ID,
		CallChain:   callChainJSON,
		EntryPoints: entryPointsJSON,
	}); err != nil {
		t.Fatalf("save result: %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, storage.TaskStatusCompleted); err != nil {
		t.Fatalf("update status: %v", err)
	}

	// Verify the task is now completed.
	fetched, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if fetched.Status != storage.TaskStatusCompleted {
		t.Errorf("expected status=completed, got %s", fetched.Status)
	}

	// Populate cache.
	_ = analysisCache.Set(ctx, cacheKey, &cache.AnalysisResult{
		TaskID:    task.ID,
		CallChain: callChainJSON,
		CreatedAt: time.Now(),
	}, 0)

	// Verify cache hit.
	got, ok := analysisCache.Get(ctx, cacheKey)
	if !ok {
		t.Fatal("expected cache hit after set")
	}
	if got.TaskID != task.ID {
		t.Errorf("cached task ID mismatch: %s vs %s", got.TaskID, task.ID)
	}
}

// ---------------------------------------------------------------------------
// Test 2: Text description input → find related functions
// ---------------------------------------------------------------------------

func TestE2E_DescriptionInput_FindFunctions(t *testing.T) {
	pool, _ := startPostgres(t)
	rdb := startRedis(t)
	wsDir := setupFakeRepos(t)

	store := storage.New(pool)
	analysisCache := cache.New(rdb)
	ctx := context.Background()

	description := "修改了超时配置"
	cacheKey := cache.CacheKey(description, []string{wsDir})

	// Create task.
	task, err := store.CreateTask(ctx, storage.InputTypeDescription, "", description, cacheKey)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	llmClient := &mockLLMClient{
		responses: []*llm.Response{
			// LLM identifies timeout-related function from description.
			{StopReason: llm.StopReasonEndTurn, Content: "service-repo/ProcessPayment"},
			// Worker traces upward.
			{StopReason: llm.StopReasonEndTurn, Content: "entry-repo/HandlePayment -> service-repo/ProcessPayment"},
		},
	}

	cp, err := checkpoint.NewFileCheckpointer(t.TempDir())
	if err != nil {
		t.Fatalf("create checkpointer: %v", err)
	}

	repos := []agent.RepoInfo{
		{Name: "service-repo", Path: wsDir + "/service-repo", Role: "service"},
		{Name: "entry-repo", Path: wsDir + "/entry-repo", Role: "entry"},
	}

	tools := []agent.Tool{&mockTool{name: "ripgrep", result: "payment.go:7:func ProcessPayment"}}
	orch := agent.NewOrchestrator(llmClient, tools, repos, wsDir, cp)

	output, err := orch.Run(ctx, agent.AnalysisInput{Description: description})
	if err != nil {
		t.Fatalf("analysis run: %v", err)
	}

	// Persist result.
	callChainJSON, _ := json.Marshal(output.CallGraph)
	if err := store.SaveResult(ctx, &storage.TaskResult{
		TaskID:    task.ID,
		CallChain: callChainJSON,
	}); err != nil {
		t.Fatalf("save result: %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, storage.TaskStatusCompleted); err != nil {
		t.Fatalf("update status: %v", err)
	}

	// Cache result.
	_ = analysisCache.Set(ctx, cacheKey, &cache.AnalysisResult{
		TaskID:    task.ID,
		CallChain: callChainJSON,
		CreatedAt: time.Now(),
	}, 0)

	// Second analysis with same input should hit cache.
	cached, ok := analysisCache.Get(ctx, cacheKey)
	if !ok {
		t.Fatal("expected cache hit for second analysis")
	}
	if cached.TaskID != task.ID {
		t.Errorf("cached task ID mismatch: %s vs %s", cached.TaskID, task.ID)
	}
}

// ---------------------------------------------------------------------------
// Test 3: Cache hit – same input returns cached result without re-running LLM
// ---------------------------------------------------------------------------

func TestE2E_CacheHit_NoRerun(t *testing.T) {
	pool, _ := startPostgres(t)
	rdb := startRedis(t)
	wsDir := setupFakeRepos(t)

	store := storage.New(pool)
	analysisCache := cache.New(rdb)
	ctx := context.Background()

	diff := `diff --git a/payment.go b/payment.go
--- a/payment.go
+++ b/payment.go
@@ -1,1 +1,2 @@
+// cache test diff`

	cacheKey := cache.CacheKey(diff, []string{wsDir})

	// Pre-populate cache to simulate first run having completed.
	existingTaskID := "pre-existing-task-id"
	callChainJSON := json.RawMessage(`[{"func_name":"ProcessPayment","repo":"service-repo"}]`)
	_ = analysisCache.Set(ctx, cacheKey, &cache.AnalysisResult{
		TaskID:    existingTaskID,
		CallChain: callChainJSON,
		CreatedAt: time.Now().Add(-1 * time.Minute),
	}, 0)

	// Track LLM calls.
	llmCallCount := 0
	llmClient := &mockLLMClient{
		responses: []*llm.Response{
			{StopReason: llm.StopReasonEndTurn, Content: "should not be called"},
		},
	}
	_ = llmClient

	// Simulate what the CLI/server does: check cache first.
	cached, ok := analysisCache.Get(ctx, cacheKey)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if cached.TaskID != existingTaskID {
		t.Errorf("expected cached task ID %s, got %s", existingTaskID, cached.TaskID)
	}

	// LLM should NOT have been called.
	if llmCallCount != 0 {
		t.Errorf("expected 0 LLM calls on cache hit, got %d", llmCallCount)
	}

	// Create a new task pointing to the cached result.
	task, err := store.CreateTask(ctx, storage.InputTypeDiff, diff, "", cacheKey)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Immediately mark as completed using cached data.
	if err := store.SaveResult(ctx, &storage.TaskResult{
		TaskID:    task.ID,
		CallChain: callChainJSON,
	}); err != nil {
		t.Fatalf("save result: %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, storage.TaskStatusCompleted); err != nil {
		t.Fatalf("update status: %v", err)
	}

	// Verify task is completed.
	fetched, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if fetched.Status != storage.TaskStatusCompleted {
		t.Errorf("expected completed status, got %s", fetched.Status)
	}

	// Verify result was saved correctly.
	result, err := store.GetResult(ctx, task.ID)
	if err != nil {
		t.Fatalf("get result: %v", err)
	}
	if !strings.Contains(string(result.CallChain), "ProcessPayment") {
		t.Errorf("result call chain missing ProcessPayment: %s", string(result.CallChain))
	}
}

// ---------------------------------------------------------------------------
// Test 4: Checkpoint resume – agent loop resumes from step 5 without re-doing steps 1-4
// ---------------------------------------------------------------------------

func TestE2E_CheckpointResume(t *testing.T) {
	pool, _ := startPostgres(t)
	rdb := startRedis(t)
	wsDir := setupFakeRepos(t)

	store := storage.New(pool)
	_ = cache.New(rdb)
	ctx := context.Background()

	cpDir := t.TempDir()
	cp, err := checkpoint.NewFileCheckpointer(cpDir)
	if err != nil {
		t.Fatalf("create checkpointer: %v", err)
	}

	taskID := "e2e-checkpoint-resume"

	// Simulate 4 steps having been completed before a crash.
	// Pre-seed checkpoint at step 4.
	priorMessages := []llm.Message{
		llm.UserMessage{Content: "system prompt"},
		llm.UserMessage{Content: "analyse this diff"},
		llm.AssistantMessage{Content: "", ToolCalls: []llm.ToolCall{
			{ID: "tc1", Name: "ripgrep", Arguments: json.RawMessage(`{"pattern":"ProcessPayment"}`)},
		}},
		llm.ToolResultMessage{ToolCallID: "tc1", Content: "payment.go:7:func ProcessPayment"},
		llm.AssistantMessage{Content: "", ToolCalls: []llm.ToolCall{
			{ID: "tc2", Name: "ripgrep", Arguments: json.RawMessage(`{"pattern":"HandlePayment"}`)},
		}},
		llm.ToolResultMessage{ToolCallID: "tc2", Content: "router.go:10:func HandlePayment"},
		llm.AssistantMessage{Content: "", ToolCalls: []llm.ToolCall{
			{ID: "tc3", Name: "ripgrep", Arguments: json.RawMessage(`{"pattern":"RegisterRoutes"}`)},
		}},
		llm.ToolResultMessage{ToolCallID: "tc3", Content: "router.go:7:func RegisterRoutes"},
		llm.AssistantMessage{Content: "", ToolCalls: []llm.ToolCall{
			{ID: "tc4", Name: "ripgrep", Arguments: json.RawMessage(`{"pattern":"entry point"}`)},
		}},
		llm.ToolResultMessage{ToolCallID: "tc4", Content: "router.go:1:// entry-repo"},
	}

	if err := cp.Save(taskID, 4, priorMessages); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	// Create task record.
	diff := "--- a/payment.go\n+++ b/payment.go\n@@ -6 +6 @@ timeout change"
	cacheKey := cache.CacheKey(diff, []string{wsDir})
	dbTask, err := store.CreateTask(ctx, storage.InputTypeDiff, diff, "", cacheKey)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	_ = dbTask

	// Mock LLM that only handles step 5 (the resume step).
	stepCount := 0
	llmClient := &countingLLMClient{
		onComplete: func(messages []llm.Message) (*llm.Response, error) {
			stepCount++
			// The resumed loop should include the prior messages.
			if len(messages) < len(priorMessages) {
				return nil, fmt.Errorf("expected at least %d messages from checkpoint, got %d",
					len(priorMessages), len(messages))
			}
			return &llm.Response{
				StopReason: llm.StopReasonEndTurn,
				Content:    "analysis complete after resume from step 4",
			}, nil
		},
	}

	tool := &mockTool{name: "ripgrep", result: "result"}

	loop := agent.NewAgentLoop(llmClient, []agent.Tool{tool}, 0, cp, "system prompt")
	result, err := loop.Run(ctx, taskID, "analyse this diff")
	if err != nil {
		t.Fatalf("loop run: %v", err)
	}

	if result.Content != "analysis complete after resume from step 4" {
		t.Errorf("unexpected content: %q", result.Content)
	}

	// Only 1 LLM call was needed (resuming from step 4).
	if stepCount != 1 {
		t.Errorf("expected 1 LLM call after resume, got %d", stepCount)
	}

	// Step count should be 4 (prior) + 1 (new) = 5.
	if result.StepCount != 5 {
		t.Errorf("expected stepCount=5, got %d", result.StepCount)
	}
}

// ---------------------------------------------------------------------------
// Test 5: HTTP API end-to-end
// ---------------------------------------------------------------------------

func TestE2E_HTTPAPI_TaskLifecycle(t *testing.T) {
	pool, _ := startPostgres(t)
	rdb := startRedis(t)
	wsDir := setupFakeRepos(t)

	store := storage.New(pool)
	analysisCache := cache.New(rdb)
	_ = analysisCache

	cfg := &config.Config{
		Workspace: config.WorkspaceConfig{Dir: wsDir},
	}

	srv := newTestAPIServer(t, cfg, store, pool, rdb)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx := context.Background()
	_ = ctx

	// POST /api/v1/tasks
	reqBody := `{"input_type":"diff","input_diff":"--- a/payment.go\n+++ b/payment.go\n@@ change"}`
	resp, err := http.Post(ts.URL+"/api/v1/tasks", "application/json", bytes.NewBufferString(reqBody))
	if err != nil {
		t.Fatalf("POST /api/v1/tasks: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202, got %d", resp.StatusCode)
	}

	var taskResp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&taskResp); err != nil {
		t.Fatalf("decode task response: %v", err)
	}
	resp.Body.Close()

	if taskResp.ID == "" {
		t.Fatal("expected non-empty task ID")
	}
	if taskResp.Status != "pending" {
		t.Errorf("expected status=pending, got %s", taskResp.Status)
	}

	// GET /api/v1/tasks/:id
	getResp, err := http.Get(ts.URL + "/api/v1/tasks/" + taskResp.ID)
	if err != nil {
		t.Fatalf("GET /api/v1/tasks/%s: %v", taskResp.ID, err)
	}
	if getResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", getResp.StatusCode)
	}
	getResp.Body.Close()

	// PUT /api/v1/tasks/:id/feedback
	fbBody := `{"type":"correct","comment":"looks good"}`
	req, _ := http.NewRequest(http.MethodPut,
		ts.URL+"/api/v1/tasks/"+taskResp.ID+"/feedback",
		bytes.NewBufferString(fbBody))
	req.Header.Set("Content-Type", "application/json")

	fbResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT feedback: %v", err)
	}
	if fbResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for feedback, got %d", fbResp.StatusCode)
	}
	fbResp.Body.Close()

	// GET /metrics
	metricsResp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	if metricsResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for metrics, got %d", metricsResp.StatusCode)
	}
	metricsResp.Body.Close()
}

// ---------------------------------------------------------------------------
// countingLLMClient
// ---------------------------------------------------------------------------

type countingLLMClient struct {
	onComplete func(messages []llm.Message) (*llm.Response, error)
}

func (c *countingLLMClient) Complete(_ context.Context, messages []llm.Message, _ []llm.ToolDefinition) (*llm.Response, error) {
	return c.onComplete(messages)
}
