-- +goose Up
-- +goose StatementBegin
-- Per API call usage ledger. Deliberately not a foreign key to sessions:
-- spend history must survive session deletion.
CREATE TABLE IF NOT EXISTS usage_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL CHECK (session_id != ''),
    provider TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'turn',  -- turn | summarize | title
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    cost REAL NOT NULL DEFAULT 0,
    estimated INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL  -- Unix timestamp in seconds
);

CREATE INDEX IF NOT EXISTS idx_usage_events_created_at ON usage_events (created_at);
CREATE INDEX IF NOT EXISTS idx_usage_events_session_id ON usage_events (session_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_usage_events_session_id;
DROP INDEX IF EXISTS idx_usage_events_created_at;
DROP TABLE IF EXISTS usage_events;
-- +goose StatementEnd
