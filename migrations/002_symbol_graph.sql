-- +goose Up
-- +goose StatementBegin

-- Symbol nodes: function/method/class/interface definitions extracted by go/ast or tree-sitter.
-- Used by DiffToSymbols (Layer B) and InMemoryGraph for deterministic call chain analysis.
CREATE TABLE IF NOT EXISTS symbol_nodes (
    id          TEXT PRIMARY KEY,   -- format: "{repo}:{file}:{qualified_name}#{arity}"
    repo        TEXT NOT NULL,
    file_path   TEXT NOT NULL,
    name        TEXT NOT NULL,      -- qualified name (e.g. "PaymentService.Handle")
    kind        TEXT NOT NULL CHECK (kind IN ('function', 'method', 'class', 'interface', 'struct', 'constant')),
    start_line  INTEGER NOT NULL,
    end_line    INTEGER NOT NULL,
    signature   TEXT,               -- function signature (parameter list)
    commit_hash TEXT NOT NULL,      -- git HEAD when this symbol was indexed
    indexed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes for common query patterns
CREATE INDEX IF NOT EXISTS idx_symbol_repo_name ON symbol_nodes(repo, name);
CREATE INDEX IF NOT EXISTS idx_symbol_file ON symbol_nodes(repo, file_path);
-- Used by DiffToSymbols: given (repo, file, line range), find overlapping symbols
CREATE INDEX IF NOT EXISTS idx_symbol_line_range ON symbol_nodes(repo, file_path, start_line, end_line);

-- Symbol edges: relationships between symbols (CALLS, IMPORTS, EXTENDS, IMPLEMENTS).
-- Loaded into InMemoryGraph at startup for microsecond-level BFS traversal.
CREATE TABLE IF NOT EXISTS symbol_edges (
    id          TEXT PRIMARY KEY,
    source_id   TEXT NOT NULL REFERENCES symbol_nodes(id) ON DELETE CASCADE,
    target_id   TEXT NOT NULL REFERENCES symbol_nodes(id) ON DELETE CASCADE,
    type        TEXT NOT NULL CHECK (type IN ('CALLS', 'IMPORTS', 'EXTENDS', 'IMPLEMENTS')),
    file_path   TEXT,               -- file where the relationship occurs
    line        INTEGER,            -- line number of the call/import site
    confidence  REAL NOT NULL DEFAULT 1.0 CHECK (confidence >= 0 AND confidence <= 1),
    UNIQUE(source_id, target_id, type)
);

-- Index for upstream traversal (find callers of a symbol)
CREATE INDEX IF NOT EXISTS idx_edge_target ON symbol_edges(target_id, type);
-- Index for downstream traversal (find callees of a symbol)
CREATE INDEX IF NOT EXISTS idx_edge_source ON symbol_edges(source_id, type);

-- Index metadata: tracks per-repo index state for staleness detection.
CREATE TABLE IF NOT EXISTS index_metadata (
    repo        TEXT PRIMARY KEY,
    commit_hash TEXT NOT NULL,       -- HEAD at last index time
    indexed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    total_files INTEGER NOT NULL DEFAULT 0,
    total_symbols INTEGER NOT NULL DEFAULT 0,
    total_edges INTEGER NOT NULL DEFAULT 0,
    language    TEXT,                -- primary language detected
    duration_ms INTEGER              -- indexing duration
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS index_metadata;
DROP TABLE IF EXISTS symbol_edges;
DROP TABLE IF EXISTS symbol_nodes;
-- +goose StatementEnd
