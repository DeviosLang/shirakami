-- +goose Up
-- +goose StatementBegin

-- contracts: discovered or manually declared cross-repo call relationships.
-- Each row is one side (provider or consumer) of a cross-repo contract.
-- Populated by 'shirakami contract scan' or hand-edited in shirakami.yaml.
CREATE TABLE IF NOT EXISTS contracts (
    id          TEXT PRIMARY KEY,
    repo        TEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('provider', 'consumer')),
    protocol    TEXT NOT NULL CHECK (protocol IN ('http', 'grpc', 'mq', 'mq_publish', 'mq_subscribe')),
    path        TEXT NOT NULL,         -- normalised HTTP path, gRPC method, or MQ topic
    method      TEXT,                  -- HTTP method (GET/POST/PUT/PATCH/DELETE) or RPC method name
    func_name   TEXT,                  -- enclosing function/handler name (best-effort)
    file_path   TEXT,                  -- source file relative to repo root
    line        INTEGER,               -- approximate line number
    symbol_id   TEXT REFERENCES symbol_nodes(id) ON DELETE SET NULL,
    commit_hash TEXT NOT NULL DEFAULT '',
    indexed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE(repo, protocol, path, COALESCE(method, ''), role)
);

CREATE INDEX IF NOT EXISTS idx_contract_repo ON contracts(repo, role);
CREATE INDEX IF NOT EXISTS idx_contract_path ON contracts(protocol, path);

-- contract_links: matched provider-consumer pairs with confidence scores.
-- Built by the Contract Registry matcher (internal/contract/matcher.go).
-- Used by Resolver.Impact() to propagate cross-repo call edges.
CREATE TABLE IF NOT EXISTS contract_links (
    id              TEXT PRIMARY KEY,
    provider_id     TEXT NOT NULL REFERENCES contracts(id) ON DELETE CASCADE,
    consumer_id     TEXT NOT NULL REFERENCES contracts(id) ON DELETE CASCADE,
    confidence      REAL NOT NULL DEFAULT 1.0 CHECK (confidence >= 0 AND confidence <= 1),
    match_type      TEXT NOT NULL CHECK (match_type IN ('exact', 'prefix', 'wildcard', 'manual')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Same provider+consumer+match_type is unique; keep highest confidence via upsert.
    UNIQUE(provider_id, consumer_id, match_type)
);

CREATE INDEX IF NOT EXISTS idx_contract_link_provider ON contract_links(provider_id);
CREATE INDEX IF NOT EXISTS idx_contract_link_consumer ON contract_links(consumer_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS contract_links;
DROP TABLE IF EXISTS contracts;
-- +goose StatementEnd
