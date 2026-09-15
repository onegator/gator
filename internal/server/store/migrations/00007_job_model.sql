-- +goose Up
-- The model a job runs on, chosen by the project's policy for its role. Empty = the backend's default.
ALTER TABLE jobs ADD COLUMN model text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE jobs DROP COLUMN model;
