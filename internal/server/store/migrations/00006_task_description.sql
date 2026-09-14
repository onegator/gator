-- +goose Up
-- What the person wants, in their words. Jobs get it in their prompt.
ALTER TABLE tasks ADD COLUMN description text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN description;
