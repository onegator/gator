-- +goose Up
-- A job whose agent is done but whose runner is still settling what it left — the checkpoint of
-- the last turn, the digest, the push — is "settling": its card says it is finishing rather than
-- jumping to done, and it is done once the receipt is in. Its time and cost count to the agent's
-- end, as before.

ALTER TABLE jobs DROP CONSTRAINT jobs_status_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_check
    CHECK (status IN ('queued', 'leased', 'running', 'stalled', 'settling', 'done', 'failed', 'stopped'));
DROP INDEX IF EXISTS jobs_runner_active_idx;
DROP INDEX IF EXISTS jobs_lease_idx;
CREATE INDEX jobs_runner_active_idx ON jobs (runner_id) WHERE status IN ('leased', 'running', 'stalled', 'settling');
CREATE INDEX jobs_lease_idx ON jobs (lease_expires_at) WHERE status IN ('leased', 'running', 'stalled', 'settling');

-- +goose Down
UPDATE jobs SET status = 'running' WHERE status = 'settling';
ALTER TABLE jobs DROP CONSTRAINT jobs_status_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_check
    CHECK (status IN ('queued', 'leased', 'running', 'stalled', 'done', 'failed', 'stopped'));
DROP INDEX IF EXISTS jobs_runner_active_idx;
DROP INDEX IF EXISTS jobs_lease_idx;
CREATE INDEX jobs_runner_active_idx ON jobs (runner_id) WHERE status IN ('leased', 'running', 'stalled');
CREATE INDEX jobs_lease_idx ON jobs (lease_expires_at) WHERE status IN ('leased', 'running', 'stalled');
