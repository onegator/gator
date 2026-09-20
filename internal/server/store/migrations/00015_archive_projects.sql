-- +goose Up
-- A project made by mistake, or one whose product is finished, stayed in the sidebar for
-- good: there was no way to remove one. Archiving hides it while keeping the history, which
-- is what append-only transitions and receipts are for.
ALTER TABLE projects ADD COLUMN archived_at timestamptz;

CREATE INDEX projects_live_idx ON projects (name) WHERE archived_at IS NULL;

-- +goose Down
DROP INDEX projects_live_idx;
ALTER TABLE projects DROP COLUMN archived_at;
