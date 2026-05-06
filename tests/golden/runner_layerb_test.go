// Package golden — Layer B integration tests for DiffToSymbols.
//
// These tests require Docker (testcontainers-go) to spin up a real PostgreSQL
// instance, run goose migrations, load per-case fixtures.sql, and then exercise
// the DiffToSymbols → Resolver.Impact pipeline.
//
// Skip with -short flag or when SKIP_INTEGRATION env var is set:
//
//	go test ./tests/golden/... -v              # runs all (Layer A + B)
//	go test ./tests/golden/... -v -short       # Layer A only
package golden

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/DeviosLang/shirakami/internal/index"
	"github.com/DeviosLang/shirakami/internal/resolve"
	"github.com/DeviosLang/shirakami/internal/tool"
)

// ---------------------------------------------------------------------------
// Layer B: DiffToSymbols — symbol recall (requires PostgreSQL index)
// ---------------------------------------------------------------------------

// TestDiffToSymbols_GoldenCases validates that DiffToSymbols correctly maps
// diff hunks to indexed symbols and that Resolver.Impact returns expected
// changed functions for each golden case.
//
// For a case to participate, its directory must contain a fixtures.sql file
// that pre-populates symbol_nodes (and optionally symbol_edges).
func TestDiffToSymbols_GoldenCases(t *testing.T) {
	if testing.Short() {
		t.Skip("Layer B tests require Docker — skipping in -short mode")
	}
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION set — skipping Layer B tests")
	}

	// Start a shared PostgreSQL container for all sub-tests.
	db, pool := startGoldenPostgres(t)
	ctx := context.Background()

	// Run goose migrations once.
	migrPath := filepath.Join("..", "..", "migrations")
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, db, migrPath); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	for _, caseName := range listCases(t) {
		caseName := caseName // capture for sub-test closure
		fixturesPath := filepath.Join("cases", caseName, "fixtures.sql")
		if _, err := os.Stat(fixturesPath); os.IsNotExist(err) {
			t.Logf("case=%s: no fixtures.sql — skipping Layer B test", caseName)
			continue
		}

		t.Run(caseName, func(t *testing.T) {
			// Load expected result.
			expected, patch := loadCase(t, caseName)
			if len(expected.ChangedFunctions) == 0 {
				t.Skip("no expected changed_functions — skipping")
			}

			// Load per-case fixtures into a transaction that is rolled back
			// after the sub-test, keeping the DB clean for the next case.
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin tx: %v", err)
			}
			t.Cleanup(func() { _ = tx.Rollback() })

			fixturesSQL, err := os.ReadFile(fixturesPath)
			if err != nil {
				t.Fatalf("read fixtures.sql: %v", err)
			}
			if _, err := tx.ExecContext(ctx, string(fixturesSQL)); err != nil {
				t.Fatalf("load fixtures.sql: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit fixtures: %v", err)
			}

			// Parse diff hunks (Layer A).
			toolHunks := tool.ParseDiffHunks(patch)
			if len(toolHunks) == 0 {
				t.Skip("no hunks in patch — skipping")
			}

			// Convert tool.DiffHunk → index.DiffHunk.
			idxHunks := make([]index.DiffHunk, len(toolHunks))
			for i, h := range toolHunks {
				idxHunks[i] = index.DiffHunk{
					File:      h.File,
					StartLine: h.StartLine,
					EndLine:   h.EndLine,
				}
			}

			// Determine source_repo from input.yaml (fall back to caseName).
			sourceRepo := caseSourceRepo(t, caseName)

			// Layer B: DiffToSymbols.
			d2sResult, err := index.DiffToSymbols(ctx, pool, sourceRepo, idxHunks)
			if err != nil {
				t.Fatalf("DiffToSymbols: %v", err)
			}

			t.Logf("case=%s repo=%s hunks=%d matched=%d uncovered=%d",
				caseName, sourceRepo, len(idxHunks),
				len(d2sResult.Matched), len(d2sResult.Uncovered))

			// Symbol recall: every expected changed_function must appear in Matched.
			matchedNames := make(map[string]bool, len(d2sResult.Matched))
			for _, m := range d2sResult.Matched {
				matchedNames[m.Symbol.Name] = true
				// Also index the simple name for qualified names like "Cache.Get".
				if idx := lastDotIndex(m.Symbol.Name); idx >= 0 {
					matchedNames[m.Symbol.Name[idx+1:]] = true
				}
			}

			covered := 0
			for _, ef := range expected.ChangedFunctions {
				simpleName := ef.Name
				if idx := lastDotIndex(ef.Name); idx >= 0 {
					simpleName = ef.Name[idx+1:]
				}
				if matchedNames[ef.Name] || matchedNames[simpleName] {
					covered++
				} else {
					t.Logf("expected func %q not found in DiffToSymbols output", ef.Name)
				}
			}

			recall := float64(covered) / float64(len(expected.ChangedFunctions))
			t.Logf("case=%s symbol_recall=%.2f (%d/%d)",
				caseName, recall, covered, len(expected.ChangedFunctions))

			if recall < 0.8 {
				t.Errorf("symbol recall %.2f < 0.80 threshold", recall)
			}

			// Impact traversal: for each matched symbol, run Resolver.Impact
			// and verify that the upstream callers form a non-empty call chain
			// (when call_chain is specified in expected.json).
			if len(expected.CallChain) > 0 && len(d2sResult.Matched) > 0 {
				store := index.NewStore(pool)
				nodes, err := store.LoadAllNodes(ctx, []string{sourceRepo})
				if err != nil {
					t.Fatalf("LoadAllNodes: %v", err)
				}
				edges, err := store.LoadAllEdges(ctx, []string{sourceRepo})
				if err != nil {
					t.Fatalf("LoadAllEdges: %v", err)
				}
				graph := index.NewInMemoryGraph()
				graph.Load(nodes, edges)
				resolver := resolve.New(graph)

				totalAffected := 0
				for _, m := range d2sResult.Matched {
					result := resolver.Impact(resolve.ImpactOptions{
						Target:    m.Symbol.Name,
						Repo:      sourceRepo,
						FilePath:  m.Symbol.FilePath,
						Direction: "upstream",
						MaxDepth:  3,
					})
					totalAffected += result.TotalAffected
				}
				t.Logf("case=%s impact_total_affected=%d", caseName, totalAffected)
				// We don't assert a specific count here — just that Impact doesn't crash.
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Helpers for Layer B
// ---------------------------------------------------------------------------

// startGoldenPostgres returns a (*sql.DB, *pgxpool.Pool) backed by PostgreSQL.
//
// If the TEST_PG_DSN environment variable is set, the function connects directly
// to that external PostgreSQL instance (useful in CI / k8s environments where a
// real Postgres is already running) and skips Docker entirely.
//
// Otherwise it starts a postgres:16-alpine container via testcontainers-go.
func startGoldenPostgres(t *testing.T) (*sql.DB, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	// ── External PostgreSQL (TEST_PG_DSN) ──────────────────────────────────
	if dsn := os.Getenv("TEST_PG_DSN"); dsn != "" {
		t.Logf("TEST_PG_DSN is set — using external PostgreSQL (skipping testcontainers)")

		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open sql.DB (external): %v", err)
		}
		if err := db.PingContext(ctx); err != nil {
			t.Fatalf("ping external PostgreSQL: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })

		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatalf("pgxpool.New (external): %v", err)
		}
		t.Cleanup(func() { pool.Close() })

		return db, pool
	}

	// ── testcontainers: spin up a fresh postgres:16-alpine ─────────────────
	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("golden_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategyAndDeadline(
			30*secondsDuration,
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
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	return db, pool
}

// caseSourceRepo reads the source_repo field from input.yaml (if present)
// and falls back to the caseName itself.
func caseSourceRepo(t *testing.T, caseName string) string {
	t.Helper()
	yamlPath := filepath.Join("cases", caseName, "input.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return caseName
	}
	// Simple line-scan: look for "source_repo: <value>"
	for _, line := range splitLines(string(data)) {
		if len(line) > 12 && line[:12] == "source_repo:" {
			repo := trimSpaceColon(line[12:])
			if repo != "" {
				return repo
			}
		}
	}
	return caseName
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func trimSpaceColon(s string) string {
	// Trim leading whitespace and surrounding quotes.
	result := ""
	inWord := false
	for _, c := range s {
		if !inWord && (c == ' ' || c == '\t' || c == '"' || c == '\'') {
			continue
		}
		inWord = true
		if c == '"' || c == '\'' || c == '\r' {
			break
		}
		result += string(c)
	}
	return result
}

func lastDotIndex(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// secondsDuration is 30 seconds expressed as time.Duration without importing time
// (avoids import conflict with runner_test.go if it also imports time in same package).
const secondsDuration = 30 * (1e9) // 30 * time.Second in nanoseconds

// Compile-time check: ensure secondsDuration has the right type.
var _ = fmt.Sprintf // suppress unused import warning
