-- +goose Up
-- Condensing the product context must never be the same as losing it. A condensed entry names
-- what it was made from, and the originals are archived rather than deleted: still readable,
-- just no longer carried into every prompt.
ALTER TABLE product_context
    ADD COLUMN archived_at timestamptz,
    ADD COLUMN condensed_from uuid[] NOT NULL DEFAULT '{}';

CREATE INDEX product_context_live_idx ON product_context (project_id, kind) WHERE archived_at IS NULL;

-- +goose Down
DROP INDEX product_context_live_idx;
ALTER TABLE product_context DROP COLUMN condensed_from, DROP COLUMN archived_at;
