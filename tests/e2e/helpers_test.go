package e2e_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Infra groups the running test infrastructure containers and clients.
type Infra struct {
	DB   *sql.DB
	Pool *pgxpool.Pool
	RDB  *redis.Client
}

// StartInfra starts a PostgreSQL container (returning both *sql.DB and
// *pgxpool.Pool) and a Redis container. All containers are terminated via
// t.Cleanup when the test finishes.
func StartInfra(t *testing.T) *Infra {
	t.Helper()
	ctx := context.Background()

	// --- PostgreSQL ---
	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("shirakami_e2e"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	pgDSN, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}

	db, err := sql.Open("pgx", pgDSN)
	if err != nil {
		t.Fatalf("sql.Open postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	// --- Redis ---
	redisContainer, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { _ = redisContainer.Terminate(ctx) })

	redisAddr, err := redisContainer.Endpoint(ctx, "")
	if err != nil {
		t.Fatalf("redis endpoint: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { _ = rdb.Close() })

	return &Infra{DB: db, Pool: pool, RDB: rdb}
}
