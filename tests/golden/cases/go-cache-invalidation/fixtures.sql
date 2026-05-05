-- fixtures.sql for golden case: go-cache-invalidation
--
-- Pre-populates symbol_nodes and symbol_edges for the shirakami repo's
-- internal/cache/cache.go symbols so that Layer B (DiffToSymbols) tests
-- can exercise the full pipeline without running the Go AST indexer.
--
-- Symbols reflect the AFTER-patch state (Cache.Get with refreshTTL param,
-- and the new Cache.Invalidate method).

INSERT INTO symbol_nodes (id, repo, file_path, name, kind, start_line, end_line, signature, commit_hash) VALUES
  -- Cache.Get (modified: refreshTTL param added at line 49)
  (
    'shirakami:internal/cache/cache.go:Cache.Get#3',
    'shirakami',
    'internal/cache/cache.go',
    'Cache.Get',
    'method',
    49, 61,
    '(ctx context.Context, key string, refreshTTL bool) (*AnalysisResult, bool)',
    'def5678'
  ),
  -- Cache.Invalidate (new method at line 62)
  (
    'shirakami:internal/cache/cache.go:Cache.Invalidate#2',
    'shirakami',
    'internal/cache/cache.go',
    'Cache.Invalidate',
    'method',
    62, 68,
    '(ctx context.Context, key string) (bool, error)',
    'def5678'
  ),
  -- Cache.Set (unchanged, present for edge context)
  (
    'shirakami:internal/cache/cache.go:Cache.Set#4',
    'shirakami',
    'internal/cache/cache.go',
    'Cache.Set',
    'method',
    70, 82,
    '(ctx context.Context, key string, result *AnalysisResult, ttl time.Duration) error',
    'def5678'
  ),
  -- orchestrator.go: analyzeWithCache calls Cache.Get
  (
    'shirakami:internal/agent/orchestrator.go:Orchestrator.analyzeWithCache#2',
    'shirakami',
    'internal/agent/orchestrator.go',
    'Orchestrator.analyzeWithCache',
    'method',
    580, 650,
    '(ctx context.Context, input AnalysisInput) (*AnalysisResult, error)',
    'def5678'
  );

-- CALLS edges: who calls Cache.Get and Cache.Invalidate
INSERT INTO symbol_edges (id, source_id, target_id, type, file_path, line, confidence) VALUES
  -- analyzeWithCache → Cache.Get
  (
    'edge:orchestrator.analyzeWithCache->cache.Get',
    'shirakami:internal/agent/orchestrator.go:Orchestrator.analyzeWithCache#2',
    'shirakami:internal/cache/cache.go:Cache.Get#3',
    'CALLS',
    'internal/agent/orchestrator.go',
    618,
    1.0
  ),
  -- Cache.Get → Cache.Set (for refreshTTL path, logical edge)
  (
    'edge:cache.Get->cache.Set',
    'shirakami:internal/cache/cache.go:Cache.Get#3',
    'shirakami:internal/cache/cache.go:Cache.Set#4',
    'CALLS',
    'internal/cache/cache.go',
    56,
    0.8
  );
