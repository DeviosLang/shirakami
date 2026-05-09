package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)
// KnowledgeRecord represents a single entry in the long-term knowledge base.
type KnowledgeRecord struct {
	ID         string
	RepoName   string
	Symbol     string
	FilePath   string
	LineNumber int
	Summary    string
	CommitHash string
	CreatedAt  time.Time
}

// Layer1 provides long-term knowledge persistence backed by PostgreSQL.
// It stores symbol-level semantic summaries and supports keyword-based
// retrieval filtered to the current HEAD commit.
type Layer1 struct {
	pool *pgxpool.Pool
}

// NewLayer1 creates a Layer1 instance backed by the given connection pool.
func NewLayer1(pool *pgxpool.Pool) *Layer1 {
	return &Layer1{pool: pool}
}

// SaveSymbolSummary persists a symbol summary to the knowledge_base table.
// If a record with the same (repo_name, symbol, commit_hash) already exists
// it is left unchanged (ON CONFLICT DO NOTHING).
func (l *Layer1) SaveSymbolSummary(ctx context.Context, repo, symbol, filePath string, line int, summary, commitHash string) error {
	const q = `
INSERT INTO knowledge_base (repo_name, symbol, file_path, line_number, summary, commit_hash)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (repo_name, symbol, commit_hash) DO NOTHING`

	_, err := l.pool.Exec(ctx, q, repo, symbol, filePath, line, summary, commitHash)
	if err != nil {
		return fmt.Errorf("layer1 save symbol summary: %w", err)
	}
	return nil
}

// SaveSymbolSummaryAsync fires SaveSymbolSummary in a background goroutine so
// it does not block the main analysis flow. Any error is silently discarded;
// callers that need error visibility should call SaveSymbolSummary directly.
func (l *Layer1) SaveSymbolSummaryAsync(repo, symbol, filePath string, line int, summary, commitHash string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = l.SaveSymbolSummary(ctx, repo, symbol, filePath, line, summary, commitHash)
	}()
}

// SearchRelevant returns up to limit knowledge records that match any of the
// provided keywords via symbol prefix matching or summary substring matching.
// Records whose commit_hash does not match currentHead are excluded so that
// stale knowledge from superseded commits is not surfaced.
func (l *Layer1) SearchRelevant(ctx context.Context, keywords []string, currentHead string, limit int) ([]KnowledgeRecord, error) {
	if len(keywords) == 0 || limit <= 0 {
		return nil, nil
	}

	// Build WHERE conditions: symbol prefix OR summary substring for each keyword.
	// We use ILIKE for case-insensitive matching on both symbol and summary.
	conditions := make([]string, 0, len(keywords))
	args := []any{currentHead} // $1 is always the commit hash filter
	argIdx := 2

	for _, kw := range keywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		conditions = append(conditions,
			fmt.Sprintf("(symbol ILIKE $%d OR summary ILIKE $%d)", argIdx, argIdx+1),
		)
		args = append(args, kw+"%", "%"+kw+"%")
		argIdx += 2
	}

	if len(conditions) == 0 {
		return nil, nil
	}

	query := fmt.Sprintf(`
SELECT id, repo_name, symbol, file_path, line_number, summary, commit_hash, created_at
FROM knowledge_base
WHERE commit_hash = $1
  AND (%s)
ORDER BY created_at DESC
LIMIT %d`, strings.Join(conditions, " OR "), limit)

	rows, err := l.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("layer1 search relevant: %w", err)
	}
	defer rows.Close()

	var records []KnowledgeRecord
	for rows.Next() {
		var r KnowledgeRecord
		if err := rows.Scan(&r.ID, &r.RepoName, &r.Symbol, &r.FilePath, &r.LineNumber, &r.Summary, &r.CommitHash, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("layer1 scan row: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("layer1 rows error: %w", err)
	}
	return records, nil
}

// ── Module Relationships (改进 8) ─────────────────────────────────────────────

// ModuleRelationship records a cross-repo call-chain edge discovered during LLM analysis.
type ModuleRelationship struct {
	ID          int64
	FromRepo    string
	FromFunc    string
	ToRepo      string
	ToFunc      string
	Confidence  float64
	SeenCount   int
	LastSeenAt  time.Time
}

// UpsertRelationship records (or reinforces) a cross-repo call relationship.
// On conflict the confidence is incremented by 0.1 (capped at 1.0), seen_count
// is incremented, and last_seen_at is refreshed.
// Errors are non-fatal — callers should log but not abort on failure.
func (l *Layer1) UpsertRelationship(ctx context.Context, fromRepo, fromFunc, toRepo, toFunc string) error {
	const q = `
INSERT INTO module_relationships (from_repo, from_func, to_repo, to_func, confidence, seen_count, last_seen_at)
VALUES ($1, $2, $3, $4, 0.1, 1, NOW())
ON CONFLICT ON CONSTRAINT uq_module_relationships DO UPDATE
  SET confidence    = LEAST(module_relationships.confidence + 0.1, 1.0),
      seen_count    = module_relationships.seen_count + 1,
      last_seen_at  = NOW()`

	_, err := l.pool.Exec(ctx, q, fromRepo, fromFunc, toRepo, toFunc)
	if err != nil {
		return fmt.Errorf("layer1 upsert relationship: %w", err)
	}
	return nil
}

// UpsertRelationshipAsync fires UpsertRelationship in a background goroutine.
// Errors are silently discarded; use UpsertRelationship directly when visibility matters.
func (l *Layer1) UpsertRelationshipAsync(fromRepo, fromFunc, toRepo, toFunc string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = l.UpsertRelationship(ctx, fromRepo, fromFunc, toRepo, toFunc)
	}()
}

// GetHighConfidenceRelationships returns the top-N cross-repo relationships where
// the callee is toRepo and confidence ≥ minConfidence. Results are ordered by
// confidence descending so the most-reliable hints appear first.
// Typical call: GetHighConfidenceRelationships(ctx, "cvm_api", 0.7, 20)
func (l *Layer1) GetHighConfidenceRelationships(ctx context.Context, toRepo string, minConfidence float64, limit int) ([]ModuleRelationship, error) {
	if limit <= 0 {
		limit = 20
	}
	const q = `
SELECT id, from_repo, from_func, to_repo, to_func, confidence, seen_count, last_seen_at
FROM module_relationships
WHERE to_repo = $1
  AND confidence >= $2
ORDER BY confidence DESC, last_seen_at DESC
LIMIT $3`

	rows, err := l.pool.Query(ctx, q, toRepo, minConfidence, limit)
	if err != nil {
		return nil, fmt.Errorf("layer1 get relationships: %w", err)
	}
	defer rows.Close()

	var rels []ModuleRelationship
	for rows.Next() {
		var r ModuleRelationship
		if err := rows.Scan(&r.ID, &r.FromRepo, &r.FromFunc, &r.ToRepo, &r.ToFunc,
			&r.Confidence, &r.SeenCount, &r.LastSeenAt); err != nil {
			return nil, fmt.Errorf("layer1 scan relationship: %w", err)
		}
		rels = append(rels, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("layer1 relationship rows error: %w", err)
	}
	return rels, nil
}

// FormatRelationshipHints returns human-readable hint strings from a slice of
// ModuleRelationship values, suitable for injection into a Worker prompt.
// Format: "  <from_repo>.<from_func> → <to_repo>.<to_func> (confidence=0.9)"
func FormatRelationshipHints(rels []ModuleRelationship) []string {
	hints := make([]string, 0, len(rels))
	for _, r := range rels {
		hints = append(hints,
			fmt.Sprintf("  %s.%s → %s.%s (confidence=%.1f)",
				r.FromRepo, r.FromFunc, r.ToRepo, r.ToFunc, r.Confidence),
		)
	}
	return hints
}

// SearchRelationships returns relationships whose from_func or to_func matches
// any of the provided keywords (case-insensitive prefix or substring search).
// Used to look up known callers of a specific function name without knowing
// which repo it lives in.
func (l *Layer1) SearchRelationships(ctx context.Context, keywords []string, minConfidence float64, limit int) ([]ModuleRelationship, error) {
	if len(keywords) == 0 || limit <= 0 {
		return nil, nil
	}

	conditions := make([]string, 0, len(keywords))
	args := []any{minConfidence} // $1
	argIdx := 2

	for _, kw := range keywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		conditions = append(conditions,
			fmt.Sprintf("(from_func ILIKE $%d OR to_func ILIKE $%d)", argIdx, argIdx+1),
		)
		args = append(args, "%"+kw+"%", "%"+kw+"%")
		argIdx += 2
	}
	if len(conditions) == 0 {
		return nil, nil
	}

	query := fmt.Sprintf(`
SELECT id, from_repo, from_func, to_repo, to_func, confidence, seen_count, last_seen_at
FROM module_relationships
WHERE confidence >= $1
  AND (%s)
ORDER BY confidence DESC, last_seen_at DESC
LIMIT %d`, strings.Join(conditions, " OR "), limit)

	rows, err := l.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("layer1 search relationships: %w", err)
	}
	defer rows.Close()

	var rels []ModuleRelationship
	for rows.Next() {
		var r ModuleRelationship
		if err := rows.Scan(&r.ID, &r.FromRepo, &r.FromFunc, &r.ToRepo, &r.ToFunc,
			&r.Confidence, &r.SeenCount, &r.LastSeenAt); err != nil {
			return nil, fmt.Errorf("layer1 scan relationship row: %w", err)
		}
		rels = append(rels, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("layer1 relationship rows error: %w", err)
	}
	return rels, nil
}
