package e2e_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/DeviosLang/shirakami/internal/cache"
	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/memory"
)

// TestAnalyzePipeline exercises the full storage / cache / checkpoint pipeline
// end-to-end using real PostgreSQL and Redis containers (via testcontainers-go).
//
// Flow:
//  1. Run goose migrations → 5 tables exist.
//  2. Insert an analysis_task row and verify the DB round-trip.
//  3. Store and retrieve an AnalysisResult via the cache layer.
//  4. Save / Load a checkpoint to verify break-point resume semantics.
//  5. Write a knowledge record via Layer1 and confirm it is searchable.
func TestAnalyzePipeline(t *testing.T) {
	infra := StartInfra(t)
	ctx := context.Background()

	// ------------------------------------------------------------------
	// Step 1 – run goose migrations.
	// ------------------------------------------------------------------
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, infra.DB, "../../migrations"); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	expectedTables := []string{
		"analysis_tasks",
		"analysis_results",
		"knowledge_base",
		"feedback",
		"metrics_daily",
	}
	for _, tbl := range expectedTables {
		var exists bool
		if err := infra.DB.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)",
			tbl,
		).Scan(&exists); err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if !exists {
			t.Errorf("table %s missing after goose up", tbl)
		}
	}

	// ------------------------------------------------------------------
	// Step 2 – storage: insert an analysis_task.
	// ------------------------------------------------------------------
	diff := "--- a/internal/payment/handler.go\n+++ b/internal/payment/handler.go\n@@ -10,6 +10,7 @@ func HandlePayment(w http.ResponseWriter, r *http.Request) {\n+\tlog.Println(\"payment received\")\n }"
	cacheKey := cache.CacheKey(diff, []string{})

	var taskID string
	err := infra.DB.QueryRowContext(ctx,
		`INSERT INTO analysis_tasks (input_type, input_diff, cache_key)
		 VALUES ($1, $2, $3) RETURNING id`,
		"diff", diff, cacheKey,
	).Scan(&taskID)
	if err != nil {
		t.Fatalf("insert analysis_task: %v", err)
	}
	if taskID == "" {
		t.Fatal("expected non-empty task ID")
	}

	// Verify the row is readable.
	var gotInputType, gotStatus string
	if err := infra.DB.QueryRowContext(ctx,
		"SELECT input_type, status FROM analysis_tasks WHERE id = $1",
		taskID,
	).Scan(&gotInputType, &gotStatus); err != nil {
		t.Fatalf("select analysis_task: %v", err)
	}
	if gotInputType != "diff" {
		t.Errorf("input_type: got %s, want diff", gotInputType)
	}
	if gotStatus != "pending" {
		t.Errorf("status: got %s, want pending", gotStatus)
	}

	// ------------------------------------------------------------------
	// Step 3 – cache: store and retrieve an AnalysisResult.
	// ------------------------------------------------------------------
	c := cache.New(infra.RDB)

	analysisResult := &cache.AnalysisResult{
		TaskID:        taskID,
		CallChain:     json.RawMessage(`["HandlePayment","processPayment","db.Save"]`),
		TestScenarios: "happy path; missing card; duplicate transaction",
		EntryPoints:   json.RawMessage(`["POST /api/v1/payments"]`),
		TokenUsage:    1500,
		StepCount:     8,
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
	}

	if err := c.Set(ctx, cacheKey, analysisResult, 0); err != nil {
		t.Fatalf("cache Set: %v", err)
	}

	got, ok := c.Get(ctx, cacheKey)
	if !ok {
		t.Fatal("expected cache hit after Set, got miss")
	}
	if got.TaskID != taskID {
		t.Errorf("cached TaskID: got %s, want %s", got.TaskID, taskID)
	}
	if got.StepCount != analysisResult.StepCount {
		t.Errorf("cached StepCount: got %d, want %d", got.StepCount, analysisResult.StepCount)
	}

	// ------------------------------------------------------------------
	// Step 4 – checkpoint: Save → Load restores the full state.
	// ------------------------------------------------------------------
	cp := checkpoint.New(infra.RDB)

	cpMessages := []llm.Message{
		llm.UserMessage{Content: diff},
		llm.AssistantMessage{Content: "analyzing call chain for HandlePayment"},
	}
	if err := cp.Save(ctx, taskID, cpMessages, 2); err != nil {
		t.Fatalf("checkpoint Save: %v", err)
	}

	state, restoredMsgs, err := cp.Load(ctx, taskID)
	if err != nil {
		t.Fatalf("checkpoint Load: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil checkpoint state")
	}
	if state.StepCount != 2 {
		t.Errorf("checkpoint StepCount: got %d, want 2", state.StepCount)
	}
	if len(restoredMsgs) != 2 {
		t.Fatalf("checkpoint messages: got %d, want 2", len(restoredMsgs))
	}
	if um, ok := restoredMsgs[0].(llm.UserMessage); !ok || um.Content != diff {
		t.Errorf("restored[0] mismatch: %+v", restoredMsgs[0])
	}

	// Clean up checkpoint (simulates successful completion).
	if err := cp.Delete(ctx, taskID); err != nil {
		t.Fatalf("checkpoint Delete: %v", err)
	}
	nilState, _, err := cp.Load(ctx, taskID)
	if err != nil {
		t.Fatalf("checkpoint Load after Delete: %v", err)
	}
	if nilState != nil {
		t.Error("expected nil state after Delete, got non-nil")
	}

	// ------------------------------------------------------------------
	// Step 5 – Layer1: save a knowledge record and retrieve it.
	// ------------------------------------------------------------------
	l1 := memory.NewLayer1(infra.Pool)
	commitHash := "e2e000abc123"

	if err := l1.SaveSymbolSummary(ctx,
		"payment-service", "HandlePayment",
		"internal/payment/handler.go", 10,
		"entry point for payment processing; validates card and calls processPayment",
		commitHash,
	); err != nil {
		t.Fatalf("Layer1 SaveSymbolSummary: %v", err)
	}

	records, err := l1.SearchRelevant(ctx, []string{"payment"}, commitHash, 5)
	if err != nil {
		t.Fatalf("Layer1 SearchRelevant: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("expected at least 1 knowledge record, got 0")
	}
	found := false
	for _, r := range records {
		if r.Symbol == "HandlePayment" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("HandlePayment not found in Layer1 results: %+v", records)
	}
}
