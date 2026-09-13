-- +goose Up
ALTER TABLE users ADD COLUMN workspace_role text NOT NULL DEFAULT 'member'
    CHECK (workspace_role IN ('admin', 'member'));

ALTER TABLE api_tokens
    ADD COLUMN name         text NOT NULL DEFAULT '',
    ADD COLUMN project_id   uuid REFERENCES projects(id) ON DELETE CASCADE,
    ADD COLUMN last_used_at timestamptz;
CREATE INDEX api_tokens_user_idx ON api_tokens (user_id) WHERE revoked_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS api_tokens_user_idx;
ALTER TABLE api_tokens DROP COLUMN last_used_at, DROP COLUMN project_id, DROP COLUMN name;
ALTER TABLE users DROP COLUMN workspace_role;
