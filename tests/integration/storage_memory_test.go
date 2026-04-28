// Package integration contains integration tests for Shirakami's storage layer.
//
// These tests require Docker to spin up real PostgreSQL and Redis containers.
// Run with:
//
//	go test ./tests/integration/... -v -count=1 -timeout=5m
package integration

import (
	"context"
	"database/sql"
	"fmt"
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

	"github.com/DeviosLang/shirakami/internal/memory"
	"github.com/DeviosLang/shirakami/internal/storage"
)

// ---------------------------------------------------------------------------
// Container helpers
// ---------------------------------------------------------------------------

func startPostgres(t *testing.T) (*pgxpool.Pool, *sql.DB, string) {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategyAndDeadline(30*time.Second,
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2)),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("create pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Also open a *sql.DB for goose.
	sqlDB, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	return pool, sqlDB, connStr
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

// runMigrations applies all goose migrations from the migrations directory.
func runMigrations(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	goose.SetBaseFS(nil) // use real filesystem
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose set dialect: %v", err)
	}
	if err := goose.Up(sqlDB, "../../migrations"); err != nil {
		t.Fatalf("goose up: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Storage layer tests
// ---------------------------------------------------------------------------

func TestStorage_CreateAndGetTask(t *testing.T) {
	pool, sqlDB, _ := startPostgres(t)
	runMigrations(t, sqlDB)

	store := storage.New(pool)
	ctx := context.Background()

	task, err := store.CreateTask(ctx, storage.InputTypeDiff, "--- a/foo.go\n+++ b/foo.go", "fix payment retry", "cache-key-123")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.ID == "" {
		t.Error("expected non-empty task ID")
	}
	if task.Status != storage.TaskStatusPending {
		t.Errorf("expected pending status, got %s", task.Status)
	}

	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ID != task.ID {
		t.Errorf("ID mismatch: %s vs %s", got.ID, task.ID)
	}
	if got.InputType != storage.InputTypeDiff {
		t.Errorf("InputType mismatch: %s", got.InputType)
	}
	if got.CacheKey != "cache-key-123" {
		t.Errorf("CacheKey mismatch: %s", got.CacheKey)
	}
}

func TestStorage_UpdateTaskStatus(t *testing.T) {
	pool, sqlDB, _ := startPostgres(t)
	runMigrations(t, sqlDB)

	store := storage.New(pool)
	ctx := context.Background()

	task, err := store.CreateTask(ctx, storage.InputTypeDescription, "", "desc", "key-456")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if err := store.UpdateTaskStatus(ctx, task.ID, storage.TaskStatusRunning); err != nil {
		t.Fatalf("UpdateTaskStatus running: %v", err)
	}
	got, _ := store.GetTask(ctx, task.ID)
	if got.Status != storage.TaskStatusRunning {
		t.Errorf("expected running, got %s", got.Status)
	}

	if err := store.UpdateTaskStatus(ctx, task.ID, storage.TaskStatusCompleted); err != nil {
		t.Fatalf("UpdateTaskStatus completed: %v", err)
	}
	got, _ = store.GetTask(ctx, task.ID)
	if got.Status != storage.TaskStatusCompleted {
		t.Errorf("expected completed, got %s", got.Status)
	}
	if got.CompletedAt == nil {
		t.Error("expected non-nil completed_at after completion")
	}
}

func TestStorage_GetTask_NotFound(t *testing.T) {
	pool, sqlDB, _ := startPostgres(t)
	runMigrations(t, sqlDB)

	store := storage.New(pool)
	ctx := context.Background()

	_, err := store.GetTask(ctx, "00000000-0000-0000-0000-000000000000")
	if err != storage.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestStorage_ListTasks(t *testing.T) {
	pool, sqlDB, _ := startPostgres(t)
	runMigrations(t, sqlDB)

	store := storage.New(pool)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := store.CreateTask(ctx, storage.InputTypeDiff, fmt.Sprintf("diff-%d", i), "", fmt.Sprintf("key-%d", i))
		if err != nil {
			t.Fatalf("CreateTask %d: %v", i, err)
		}
	}

	tasks, err := store.ListTasks(ctx, 10)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) < 3 {
		t.Errorf("expected at least 3 tasks, got %d", len(tasks))
	}
}

// ---------------------------------------------------------------------------
// Layer1 (long-term knowledge base) tests
// ---------------------------------------------------------------------------

func TestLayer1_SaveAndSearch(t *testing.T) {
	pool, sqlDB, _ := startPostgres(t)
	runMigrations(t, sqlDB)

	l1 := memory.NewLayer1(pool)
	ctx := context.Background()
	commitHash := "abc123"

	if err := l1.SaveSymbolSummary(ctx, "payment-service", "PaymentService.Execute",
		"service/payment.go", 42, "handles payment processing and retry", commitHash); err != nil {
		t.Fatalf("SaveSymbolSummary: %v", err)
	}
	if err := l1.SaveSymbolSummary(ctx, "order-service", "OrderService.UpdateStatus",
		"service/order.go", 88, "updates order status in database", commitHash); err != nil {
		t.Fatalf("SaveSymbolSummary 2: %v", err)
	}

	// Search by keyword matching symbol prefix.
	records, err := l1.SearchRelevant(ctx, []string{"PaymentService"}, commitHash, 10)
	if err != nil {
		t.Fatalf("SearchRelevant: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].Symbol != "PaymentService.Execute" {
		t.Errorf("unexpected symbol: %s", records[0].Symbol)
	}

	// Search by summary substring.
	records, err = l1.SearchRelevant(ctx, []string{"retry"}, commitHash, 10)
	if err != nil {
		t.Fatalf("SearchRelevant by summary: %v", err)
	}
	if len(records) == 0 {
		t.Error("expected at least 1 record matching 'retry' in summary")
	}
}

func TestLayer1_StaleCommitFiltered(t *testing.T) {
	pool, sqlDB, _ := startPostgres(t)
	runMigrations(t, sqlDB)

	l1 := memory.NewLayer1(pool)
	ctx := context.Background()
	oldHash := "old-hash-111"

	if err := l1.SaveSymbolSummary(ctx, "svc", "OldFunc", "old.go", 1, "old summary", oldHash); err != nil {
		t.Fatalf("SaveSymbolSummary: %v", err)
	}

	// Search with a different (new) commit hash — old record should not appear.
	records, err := l1.SearchRelevant(ctx, []string{"OldFunc"}, "new-hash-222", 10)
	if err != nil {
		t.Fatalf("SearchRelevant: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("expected 0 records for stale commit, got %d", len(records))
	}
}

func TestLayer1_ConflictIdempotent(t *testing.T) {
	pool, sqlDB, _ := startPostgres(t)
	runMigrations(t, sqlDB)

	l1 := memory.NewLayer1(pool)
	ctx := context.Background()
	hash := "hash-xyz"

	// Saving the same (repo, symbol, commit_hash) twice should not error.
	for i := 0; i < 2; i++ {
		if err := l1.SaveSymbolSummary(ctx, "svc", "MyFunc", "my.go", 10, "summary", hash); err != nil {
			t.Fatalf("SaveSymbolSummary attempt %d: %v", i, err)
		}
	}

	records, err := l1.SearchRelevant(ctx, []string{"MyFunc"}, hash, 10)
	if err != nil {
		t.Fatalf("SearchRelevant: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("expected exactly 1 record after idempotent saves, got %d", len(records))
	}
}
