-- Migration 006: module_relationships
-- Stores cross-repo call-chain relationships discovered during LLM analysis.
-- Used by Layer1 to inject high-confidence known relationships into Worker prompts,
-- reducing redundant ripgrep searches on repeat analyses.
--
-- from_repo/from_func: the caller side (the repo that WAS analysed, e.g. vstation_compute_access)
-- to_repo/to_func:     the callee side (the repo being called, e.g. cvm_api)
-- confidence:          cumulative score; incremented by 0.1 per observation, capped at 1.0
-- seen_count:          raw observation count
-- last_seen_at:        timestamp of most recent observation

CREATE TABLE IF NOT EXISTS module_relationships (
    id             BIGSERIAL PRIMARY KEY,
    from_repo      TEXT        NOT NULL,
    from_func      TEXT        NOT NULL,
    to_repo        TEXT        NOT NULL,
    to_func        TEXT        NOT NULL,
    confidence     FLOAT       NOT NULL DEFAULT 0.1,
    seen_count     INT         NOT NULL DEFAULT 1,
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_module_relationships UNIQUE (from_repo, from_func, to_repo, to_func)
);

-- Index for fast querying by to_repo (Worker injection path).
CREATE INDEX IF NOT EXISTS idx_module_relationships_to_repo
    ON module_relationships (to_repo, confidence DESC);

-- Index for fast querying by from_repo (Orchestrator write-back path).
CREATE INDEX IF NOT EXISTS idx_module_relationships_from_repo
    ON module_relationships (from_repo, last_seen_at DESC);
