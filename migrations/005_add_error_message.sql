-- +goose Up
-- +goose StatementBegin

-- analysis_tasks: store the failure reason so the API can surface it to callers
ALTER TABLE analysis_tasks
    ADD COLUMN IF NOT EXISTS error_message TEXT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE analysis_tasks
    DROP COLUMN IF EXISTS error_message;

-- +goose StatementEnd
