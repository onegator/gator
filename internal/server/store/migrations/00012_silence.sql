-- +goose Up
-- A queued job nobody can take, and a runner that stopped answering, were both silent states:
-- the work simply waited. These columns give that silence a name and a start time.
ALTER TABLE jobs ADD COLUMN unassignable_since timestamptz;

ALTER TABLE runners
    ADD COLUMN offline_reason text NOT NULL DEFAULT '',
    ADD COLUMN offline_since  timestamptz;

-- Runners that are already offline have been so since their last sign of life.
UPDATE runners SET offline_since = last_heartbeat_at WHERE status = 'offline';

-- +goose Down
ALTER TABLE runners DROP COLUMN offline_since, DROP COLUMN offline_reason;
ALTER TABLE jobs DROP COLUMN unassignable_since;
