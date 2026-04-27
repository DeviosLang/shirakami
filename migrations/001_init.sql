-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS analysis_tasks (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    input_type  VARCHAR(20) NOT NULL CHECK (input_type IN ('diff', 'description', 'combined')),
    input_diff  TEXT,
    input_desc  TEXT,
    cache_key   VARCHAR(64),
    status      VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS analysis_results (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id      UUID NOT NULL REFERENCES analysis_tasks(id) ON DELETE CASCADE,
    call_chain   JSONB,
    test_scenarios TEXT,
    entry_points JSONB,
    token_usage  INTEGER,
    step_count   INTEGER,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS knowledge_base (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_name   VARCHAR(255) NOT NULL,
    symbol      VARCHAR(512) NOT NULL,
    file_path   TEXT NOT NULL,
    line_number INTEGER,
    summary     TEXT,
    commit_hash VARCHAR(40) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (repo_name, symbol, commit_hash)
);

CREATE TABLE IF NOT EXISTS feedback (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id    UUID NOT NULL REFERENCES analysis_tasks(id) ON DELETE CASCADE,
    type       VARCHAR(30) NOT NULL CHECK (type IN ('false_positive', 'false_negative', 'correct')),
    comment    TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS metrics_daily (
    date            DATE PRIMARY KEY,
    total_tasks     INTEGER NOT NULL DEFAULT 0,
    avg_steps       NUMERIC(10, 2),
    avg_tokens      NUMERIC(10, 2),
    cache_hit_rate  NUMERIC(5, 4)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS metrics_daily;
DROP TABLE IF EXISTS feedback;
DROP TABLE IF EXISTS knowledge_base;
DROP TABLE IF EXISTS analysis_results;
DROP TABLE IF EXISTS analysis_tasks;
-- +goose StatementEnd
