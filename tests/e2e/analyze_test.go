// Package e2e contains end-to-end integration tests for Shirakami.
package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/DeviosLang/shirakami/internal/cache"
	"github.com/DeviosLang/shirakami/internal/checkpoint"
	"github.com/DeviosLang/shirakami/internal/llm"
	"github.com/DeviosLang/shirakami/internal/memory"
	"github.com/DeviosLang/shirakami/internal/storage"
)

// TestAnalyzePipeline_StorageCacheCheckpointLayer1 exercises the core data
// pipeline end-to-end using real PostgreSQL and Redis containers:
//  1. goose migrations → 5 tables
//  2. storage.Store CRUD round-trip
//  3. cache Set / Get / TTL
//  4. checkpoint Save → Load → Delete
//  5. Layer1 SaveSymbolSummary → SearchRelevant
func TestAnalyzePipeline_StorageCacheCheckpointLayer1(t *testing.T) {
	pool, pgDSN := startPostgres(t)
	rdb := startRedis(t)
	ctx := context.Background()

	// --- Step 1: run migrations ---
	migrPath := migrationsPath(t)

	db, err := sql.Open("pgx", pgDSN)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, db, migrPath); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	tables := []string{"analysis_tasks", "analysis_results", "knowledge_base", "feedback", "metrics_daily"}
	for _, tbl := range tables {
		var exists bool
		if err := db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)",
			tbl,
		).Scan(&exists); err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if !exists {
			t.Errorf("table %s missing after goose up", tbl)
		}
	}

	// --- Step 2: storage CRUD ---
	store := storage.New(pool)

	diff := "--- a/payment.go\n+++ b/payment.go\n@@ -5,0 +6 @@ func Pay() {\n+\tlog.Println(\"pay\")\n }"
	cacheKey := cache.CacheKey(diff, []string{})

	task, err := store.CreateTask(ctx, storage.InputTypeDiff, diff, "", cacheKey)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.Status != storage.TaskStatusPending {
		t.Errorf("status: got %s, want pending", task.Status)
	}

	if err := store.UpdateTaskStatus(ctx, task.ID, storage.TaskStatusRunning); err != nil {
		t.Fatalf("UpdateTaskStatus running: %v", err)
	}
	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != storage.TaskStatusRunning {
		t.Errorf("status after update: got %s, want running", got.Status)
	}

	// --- Step 3: cache Set / Get / TTL ---
	c := cache.New(rdb)

	analysisResult := &cache.AnalysisResult{
		TaskID:        task.ID,
		CallChain:     json.RawMessage(`["Pay","processPayment","db.Save"]`),
		TestScenarios: "happy path; nil input; timeout",
		EntryPoints:   json.RawMessage(`["POST /api/v1/pay"]`),
		TokenUsage:    1200,
		StepCount:     6,
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
	}

	if err := c.Set(ctx, cacheKey, analysisResult, 0); err != nil {
		t.Fatalf("cache Set: %v", err)
	}

	cachedResult, ok := c.Get(ctx, cacheKey)
	if !ok {
		t.Fatal("expected cache hit, got miss")
	}
	if cachedResult.TaskID != task.ID {
		t.Errorf("cached TaskID: got %s, want %s", cachedResult.TaskID, task.ID)
	}

	// Verify TTL behaviour with a short-lived entry.
	shortKey := cache.CacheKey("short-lived-pipeline", []string{})
	if err := c.Set(ctx, shortKey, &cache.AnalysisResult{TaskID: "short"}, 80*time.Millisecond); err != nil {
		t.Fatalf("cache Set short: %v", err)
	}
	if _, ok := c.Get(ctx, shortKey); !ok {
		t.Fatal("expected hit before TTL expiry")
	}
	time.Sleep(150 * time.Millisecond)
	if _, ok := c.Get(ctx, shortKey); ok {
		t.Error("expected miss after TTL expiry, got hit")
	}

	// --- Step 4: checkpoint Save → Load → Delete ---
	cp := checkpoint.New(rdb)
	cpMessages := []llm.Message{
		llm.UserMessage{Content: diff},
		llm.AssistantMessage{Content: "tracing call chain for Pay"},
	}
	if err := cp.Save(ctx, task.ID, cpMessages, 2); err != nil {
		t.Fatalf("checkpoint Save: %v", err)
	}

	cpState, restoredMsgs, err := cp.Load(ctx, task.ID)
	if err != nil {
		t.Fatalf("checkpoint Load: %v", err)
	}
	if cpState == nil {
		t.Fatal("expected non-nil checkpoint state")
	}
	if cpState.StepCount != 2 {
		t.Errorf("StepCount: got %d, want 2", cpState.StepCount)
	}
	if len(restoredMsgs) != 2 {
		t.Fatalf("restored messages: got %d, want 2", len(restoredMsgs))
	}

	if err := cp.Delete(ctx, task.ID); err != nil {
		t.Fatalf("checkpoint Delete: %v", err)
	}
	nilState, _, err := cp.Load(ctx, task.ID)
	if err != nil {
		t.Fatalf("Load after Delete: %v", err)
	}
	if nilState != nil {
		t.Error("expected nil state after Delete")
	}

	// --- Step 5: Layer1 knowledge record ---
	l1 := memory.NewLayer1(pool)
	commitHash := "e2eabc123"

	if err := l1.SaveSymbolSummary(ctx,
		"payment-service", "Pay",
		"internal/payment/pay.go", 5,
		"entry point for payment flow; calls processPayment",
		commitHash,
	); err != nil {
		t.Fatalf("Layer1 SaveSymbolSummary: %v", err)
	}

	records, err := l1.SearchRelevant(ctx, []string{"Pay"}, commitHash, 5)
	if err != nil {
		t.Fatalf("Layer1 SearchRelevant: %v", err)
	}
	found := false
	for _, r := range records {
		if r.Symbol == "Pay" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'Pay' in Layer1 results, got %+v", records)
	}

	// Stale commit must not appear.
	staleRecords, err := l1.SearchRelevant(ctx, []string{"Pay"}, "different-hash", 5)
	if err != nil {
		t.Fatalf("Layer1 SearchRelevant stale: %v", err)
	}
	if len(staleRecords) != 0 {
		t.Errorf("expected 0 results for stale hash, got %d", len(staleRecords))
	}
}
