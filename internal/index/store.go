// Package index provides deterministic symbol indexing for Go repositories
// using go/ast + go/types, and stores the results in PostgreSQL for
// subsequent graph traversal by the resolve package.
package index

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SymbolNode represents a code symbol (function, method, class, etc.)
type SymbolNode struct {
	ID         string    // "{repo}:{file}:{qualified_name}#{arity}"
	Repo       string
	FilePath   string
	Name       string // qualified name (e.g. "PaymentService.Handle")
	Kind       string // function / method / class / interface / struct / constant
	StartLine  int
	EndLine    int
	Signature  string
	CommitHash string
	IndexedAt  time.Time
}

// SymbolEdge represents a relationship between two symbols.
type SymbolEdge struct {
	ID         string
	SourceID   string
	TargetID   string
	Type       string // CALLS / IMPORTS / EXTENDS / IMPLEMENTS
	FilePath   string
	Line       int
	Confidence float64
}

// IndexMetadata tracks per-repo index state.
type IndexMetadata struct {
	Repo         string
	CommitHash   string
	IndexedAt    time.Time
	TotalFiles   int
	TotalSymbols int
	TotalEdges   int
	Language     string
	DurationMs   int
}

// Store handles persistence of symbol index data to PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates an index store backed by the given connection pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SaveNodes bulk-inserts symbol nodes (upsert on conflict).
func (s *Store) SaveNodes(ctx context.Context, nodes []SymbolNode) error {
	if len(nodes) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("index store begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, n := range nodes {
		_, err := tx.Exec(ctx, `
			INSERT INTO symbol_nodes (id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (id) DO UPDATE SET
				start_line = EXCLUDED.start_line,
				end_line = EXCLUDED.end_line,
				signature = EXCLUDED.signature,
				commit_hash = EXCLUDED.commit_hash,
				indexed_at = NOW()`,
			n.ID, n.Repo, n.FilePath, n.Name, n.Kind, n.StartLine, n.EndLine, n.Signature, n.CommitHash,
		)
		if err != nil {
			return fmt.Errorf("index store save node %s: %w", n.ID, err)
		}
	}

	return tx.Commit(ctx)
}

// SaveEdges bulk-inserts symbol edges (upsert on conflict).
func (s *Store) SaveEdges(ctx context.Context, edges []SymbolEdge) error {
	if len(edges) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("index store begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, e := range edges {
		_, err := tx.Exec(ctx, `
			INSERT INTO symbol_edges (id, source_id, target_id, type, file_path, line, confidence)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (source_id, target_id, type) DO UPDATE SET
				file_path = EXCLUDED.file_path,
				line = EXCLUDED.line,
				confidence = EXCLUDED.confidence`,
			e.ID, e.SourceID, e.TargetID, e.Type, e.FilePath, e.Line, e.Confidence,
		)
		if err != nil {
			return fmt.Errorf("index store save edge %s: %w", e.ID, err)
		}
	}

	return tx.Commit(ctx)
}

// SaveMetadata upserts index metadata for a repo.
func (s *Store) SaveMetadata(ctx context.Context, meta IndexMetadata) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO index_metadata (repo, commit_hash, indexed_at, total_files, total_symbols, total_edges, language, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (repo) DO UPDATE SET
			commit_hash = EXCLUDED.commit_hash,
			indexed_at = EXCLUDED.indexed_at,
			total_files = EXCLUDED.total_files,
			total_symbols = EXCLUDED.total_symbols,
			total_edges = EXCLUDED.total_edges,
			language = EXCLUDED.language,
			duration_ms = EXCLUDED.duration_ms`,
		meta.Repo, meta.CommitHash, meta.IndexedAt, meta.TotalFiles, meta.TotalSymbols, meta.TotalEdges, meta.Language, meta.DurationMs,
	)
	if err != nil {
		return fmt.Errorf("index store save metadata: %w", err)
	}
	return nil
}

// GetMetadata returns index metadata for a repo, or nil if not indexed.
func (s *Store) GetMetadata(ctx context.Context, repo string) (*IndexMetadata, error) {
	var m IndexMetadata
	err := s.pool.QueryRow(ctx, `
		SELECT repo, commit_hash, indexed_at, total_files, total_symbols, total_edges, language, duration_ms
		FROM index_metadata WHERE repo = $1`, repo,
	).Scan(&m.Repo, &m.CommitHash, &m.IndexedAt, &m.TotalFiles, &m.TotalSymbols, &m.TotalEdges, &m.Language, &m.DurationMs)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("index store get metadata: %w", err)
	}
	return &m, nil
}

// DeleteByRepo removes all symbols and edges for a repo (used before full reindex).
func (s *Store) DeleteByRepo(ctx context.Context, repo string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM symbol_nodes WHERE repo = $1`, repo)
	if err != nil {
		return fmt.Errorf("index store delete repo %s: %w", repo, err)
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM index_metadata WHERE repo = $1`, repo)
	return err
}

// DeleteByFile removes symbols and edges for specific files (incremental reindex).
func (s *Store) DeleteByFile(ctx context.Context, repo string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM symbol_nodes WHERE repo = $1 AND file_path = ANY($2)`, repo, files)
	return err
}

// LoadAllEdges loads all edges for given repos into memory (for InMemoryGraph).
func (s *Store) LoadAllEdges(ctx context.Context, repos []string) ([]SymbolEdge, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.source_id, e.target_id, e.type, e.file_path, e.line, e.confidence
		FROM symbol_edges e
		JOIN symbol_nodes n ON n.id = e.source_id
		WHERE n.repo = ANY($1)`, repos,
	)
	if err != nil {
		return nil, fmt.Errorf("index store load edges: %w", err)
	}
	defer rows.Close()

	var edges []SymbolEdge
	for rows.Next() {
		var e SymbolEdge
		if err := rows.Scan(&e.ID, &e.SourceID, &e.TargetID, &e.Type, &e.FilePath, &e.Line, &e.Confidence); err != nil {
			return nil, fmt.Errorf("index store scan edge: %w", err)
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// LoadAllNodes loads all nodes for given repos into memory.
func (s *Store) LoadAllNodes(ctx context.Context, repos []string) ([]SymbolNode, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash, indexed_at
		FROM symbol_nodes WHERE repo = ANY($1)`, repos,
	)
	if err != nil {
		return nil, fmt.Errorf("index store load nodes: %w", err)
	}
	defer rows.Close()

	var nodes []SymbolNode
	for rows.Next() {
		var n SymbolNode
		if err := rows.Scan(&n.ID, &n.Repo, &n.FilePath, &n.Name, &n.Kind, &n.StartLine, &n.EndLine, &n.Signature, &n.CommitHash, &n.IndexedAt); err != nil {
			return nil, fmt.Errorf("index store scan node: %w", err)
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// FindSymbolsByLineRange finds symbols in a file that overlap with the given line range.
// Used by DiffToSymbols (Layer B) to map diff hunks to symbol definitions.
func (s *Store) FindSymbolsByLineRange(ctx context.Context, repo, filePath string, startLine, endLine int) ([]SymbolNode, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash, indexed_at
		FROM symbol_nodes
		WHERE repo = $1 AND file_path = $2
		  AND start_line <= $4 AND end_line >= $3`,
		repo, filePath, startLine, endLine,
	)
	if err != nil {
		return nil, fmt.Errorf("index store find by line range: %w", err)
	}
	defer rows.Close()

	var nodes []SymbolNode
	for rows.Next() {
		var n SymbolNode
		if err := rows.Scan(&n.ID, &n.Repo, &n.FilePath, &n.Name, &n.Kind, &n.StartLine, &n.EndLine, &n.Signature, &n.CommitHash, &n.IndexedAt); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}
