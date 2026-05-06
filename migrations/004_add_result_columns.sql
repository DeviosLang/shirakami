-- +goose Up
-- +goose StatementBegin

-- analysis_tasks: add modes, source_repo, queue_position
ALTER TABLE analysis_tasks
    ADD COLUMN IF NOT EXISTS modes         TEXT[]       NOT NULL DEFAULT ARRAY['chain','e2e','ut'],
    ADD COLUMN IF NOT EXISTS source_repo   VARCHAR(255),
    ADD COLUMN IF NOT EXISTS queue_position INTEGER;

-- analysis_results: add extended result columns
ALTER TABLE analysis_results
    ADD COLUMN IF NOT EXISTS ut_suggestions    TEXT,
    ADD COLUMN IF NOT EXISTS function_analyses JSONB,
    ADD COLUMN IF NOT EXISTS impact_summary    TEXT,
    ADD COLUMN IF NOT EXISTS cross_repo_hops   INTEGER,
    ADD COLUMN IF NOT EXISTS risk              VARCHAR(10),
    ADD COLUMN IF NOT EXISTS index_coverage    JSONB,
    ADD COLUMN IF NOT EXISTS modes             TEXT[];

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE analysis_results
    DROP COLUMN IF EXISTS modes,
    DROP COLUMN IF EXISTS index_coverage,
    DROP COLUMN IF EXISTS risk,
    DROP COLUMN IF EXISTS cross_repo_hops,
    DROP COLUMN IF EXISTS impact_summary,
    DROP COLUMN IF EXISTS function_analyses,
    DROP COLUMN IF EXISTS ut_suggestions;

ALTER TABLE analysis_tasks
    DROP COLUMN IF EXISTS queue_position,
    DROP COLUMN IF EXISTS source_repo,
    DROP COLUMN IF EXISTS modes;

-- +goose StatementEnd
