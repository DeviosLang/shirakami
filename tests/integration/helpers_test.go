package integration

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startPostgresWithPool starts a PostgreSQL container and returns both a
// *sql.DB (for goose) and a *pgxpool.Pool (for the storage/memory layers).
func startPostgresWithPool(t *testing.T) (*sql.DB, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("shirakami_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategyAndDeadline(30*time.Second,
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

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("create pgxpool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	return db, pool
}

// startRedisClient starts a Redis container and returns a *redis.Client.
// Named differently from the existing startRedis to avoid duplicate declarations.
func startRedisClient(t *testing.T) *redis.Client {
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
