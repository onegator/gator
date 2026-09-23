-- +goose Up
-- When a runner is finished with, it is forgotten, not erased. jobs.runner_id is ON DELETE
-- SET NULL, so deleting the row would take with it the record of which machine did the work —
-- and reading back what happened is the point of the whole system. A forgotten runner leaves
-- the list, its token stops authenticating, and every job it ever ran still knows its name.
ALTER TABLE runners ADD COLUMN retired_at timestamptz;

-- +goose Down
ALTER TABLE runners DROP COLUMN retired_at;
