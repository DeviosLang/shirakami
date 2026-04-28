package integration

import (
	"context"
	"testing"

	"github.com/DeviosLang/shirakami/internal/memory"
)

// setupLayer1DB creates a fresh PostgreSQL container with the knowledge_base
// table and returns a *memory.Layer1 backed by the pool.
func setupLayer1DB(t *testing.T) *memory.Layer1 {
	t.Helper()
	ctx := context.Background()

	_, pool := startPostgresWithPool(t)

	// Create only the knowledge_base table for Layer1 tests.
	_, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS knowledge_base (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_name   VARCHAR(255) NOT NULL,
    symbol      VARCHAR(512) NOT NULL,
    file_path   TEXT NOT NULL,
    line_number INTEGER,
    summary     TEXT,
    commit_hash VARCHAR(40) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (repo_name, symbol, commit_hash)
)`)
	if err != nil {
		t.Fatalf("create knowledge_base table: %v", err)
	}

	return memory.NewLayer1(pool)
}

func TestLayer1_WriteAndSearchByKeyword(t *testing.T) {
	l1 := setupLayer1DB(t)
	ctx := context.Background()

	commitHash := "abc123def456"

	if err := l1.SaveSymbolSummary(ctx,
		"my-repo", "HandlePayment", "internal/payment/handler.go", 42,
		"handles payment processing and validation", commitHash,
	); err != nil {
		t.Fatalf("SaveSymbolSummary HandlePayment: %v", err)
	}
	if err := l1.SaveSymbolSummary(ctx,
		"my-repo", "ProcessOrder", "internal/order/processor.go", 100,
		"processes order creation and inventory update", commitHash,
	); err != nil {
		t.Fatalf("SaveSymbolSummary ProcessOrder: %v", err)
	}

	// Search by keyword matching symbol prefix.
	results, err := l1.SearchRelevant(ctx, []string{"Handle"}, commitHash, 10)
	if err != nil {
		t.Fatalf("SearchRelevant Handle: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'Handle', got %d", len(results))
	}
	if results[0].Symbol != "HandlePayment" {
		t.Errorf("Symbol: got %s, want HandlePayment", results[0].Symbol)
	}

	// Search by keyword matching summary substring.
	results, err = l1.SearchRelevant(ctx, []string{"inventory"}, commitHash, 10)
	if err != nil {
		t.Fatalf("SearchRelevant inventory: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'inventory', got %d", len(results))
	}
	if results[0].Symbol != "ProcessOrder" {
		t.Errorf("Symbol: got %s, want ProcessOrder", results[0].Symbol)
	}
}

func TestLayer1_OldRecordsFilteredByCommitHash(t *testing.T) {
	l1 := setupLayer1DB(t)
	ctx := context.Background()

	oldHash := "old000000000"
	newHash := "new111111111"

	if err := l1.SaveSymbolSummary(ctx,
		"my-repo", "OldHandler", "internal/old.go", 1,
		"old implementation of the handler", oldHash,
	); err != nil {
		t.Fatalf("SaveSymbolSummary OldHandler: %v", err)
	}
	if err := l1.SaveSymbolSummary(ctx,
		"my-repo", "NewHandler", "internal/new.go", 1,
		"new implementation of the handler", newHash,
	); err != nil {
		t.Fatalf("SaveSymbolSummary NewHandler: %v", err)
	}

	// Querying with newHash must exclude the old record.
	results, err := l1.SearchRelevant(ctx, []string{"Handler"}, newHash, 10)
	if err != nil {
		t.Fatalf("SearchRelevant: %v", err)
	}
	for _, r := range results {
		if r.CommitHash == oldHash {
			t.Errorf("stale record (commit %s) must not appear for head %s", oldHash, newHash)
		}
	}
	if len(results) != 1 || results[0].Symbol != "NewHandler" {
		t.Errorf("expected exactly [NewHandler], got %+v", results)
	}
}

func TestLayer1_EmptyKeywordsReturnsNil(t *testing.T) {
	l1 := setupLayer1DB(t)
	ctx := context.Background()

	results, err := l1.SearchRelevant(ctx, []string{}, "any-hash", 10)
	if err != nil {
		t.Fatalf("SearchRelevant with empty keywords: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for empty keywords, got %d", len(results))
	}
}
