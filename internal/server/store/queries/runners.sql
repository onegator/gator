-- name: UpsertRunner :one
INSERT INTO runners (token_id, name, location, status, capabilities, protocol_version, binary_version, connected_at, last_heartbeat_at)
VALUES ($1, $2, $3, 'online', $4, $5, $6, now(), now())
ON CONFLICT (token_id) DO UPDATE
SET name = EXCLUDED.name, location = EXCLUDED.location, status = 'online',
    capabilities = EXCLUDED.capabilities, protocol_version = EXCLUDED.protocol_version,
    binary_version = EXCLUDED.binary_version, connected_at = now(), last_heartbeat_at = now(), updated_at = now()
RETURNING *;

-- name: RunnerHeartbeat :exec
UPDATE runners SET last_heartbeat_at = now(), auth_state = $2, status = $3, updated_at = now() WHERE id = $1;

-- name: SetRunnerStatus :exec
UPDATE runners SET status = $2, updated_at = now() WHERE id = $1;

-- name: MarkSilentRunnersOffline :many
UPDATE runners SET status = 'offline', updated_at = now()
WHERE status <> 'offline' AND last_heartbeat_at < sqlc.arg(before)
RETURNING id, name;

-- name: ListRunners :many
SELECT * FROM runners ORDER BY name;

-- name: GetRunner :one
SELECT * FROM runners WHERE id = $1;

-- name: CreateJob :one
INSERT INTO jobs (task_id, project_id, phase, role, backend, instruction, bounds, max_attempts, created_by_kind, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = $1;

-- name: GetJobForUpdate :one
SELECT * FROM jobs WHERE id = $1 FOR UPDATE;

-- name: ListJobsByTask :many
SELECT * FROM jobs WHERE task_id = $1 ORDER BY created_at;

-- name: LeaseJobs :many
-- Hands up to `slots` queued jobs to a runner. SKIP LOCKED lets concurrent runners lease
-- disjoint jobs without waiting on each other.
UPDATE jobs
SET status = 'leased', runner_id = sqlc.arg(runner_id), lease_expires_at = sqlc.arg(lease_until),
    attempts = attempts + 1, updated_at = now()
WHERE id IN (
    SELECT j.id FROM jobs j
    WHERE j.status = 'queued'
      AND j.backend = ANY(sqlc.arg(backends)::text[])
      AND (cardinality(sqlc.arg(projects)::uuid[]) = 0 OR j.project_id = ANY(sqlc.arg(projects)::uuid[]))
    ORDER BY j.created_at
    LIMIT sqlc.arg(slots)
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: ExtendLeases :execrows
UPDATE jobs SET lease_expires_at = sqlc.arg(lease_until), updated_at = now()
WHERE runner_id = sqlc.arg(runner_id) AND id = ANY(sqlc.arg(job_ids)::uuid[])
  AND status IN ('leased', 'running', 'stalled');

-- name: MarkJobActive :one
-- First event moves leased → running; any event revives a stalled job.
UPDATE jobs
SET status = 'running', started_at = COALESCE(started_at, now()), last_event_at = now(), updated_at = now()
WHERE id = $1 AND runner_id = $2 AND status IN ('leased', 'running', 'stalled')
RETURNING *;

-- name: InsertJobEvent :execrows
INSERT INTO job_events (job_id, seq, type, payload, at) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (job_id, seq) DO NOTHING;

-- name: ListJobEvents :many
SELECT * FROM job_events WHERE job_id = $1 AND seq > $2 ORDER BY seq LIMIT $3;

-- name: FinishJob :exec
UPDATE jobs
SET status = $2, receipt = $3, stop_reason = $4, finished_at = now(), lease_expires_at = NULL, updated_at = now()
WHERE id = $1;

-- name: RequeueExpiredLeases :many
-- A lease nobody extended means the runner lost the job. Retry until max_attempts, then fail.
UPDATE jobs
SET status = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
    stop_reason = CASE WHEN attempts >= max_attempts THEN 'lease expired after ' || attempts || ' attempts' ELSE stop_reason END,
    runner_id = CASE WHEN attempts >= max_attempts THEN runner_id ELSE NULL END,
    finished_at = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END,
    lease_expires_at = NULL, updated_at = now()
WHERE status IN ('leased', 'running', 'stalled') AND lease_expires_at < sqlc.arg(now)
RETURNING id, task_id, status;

-- name: MarkStalledJobs :many
UPDATE jobs SET status = 'stalled', updated_at = now()
WHERE status = 'running' AND last_event_at < sqlc.arg(before)
RETURNING id, task_id;

-- name: StopQueuedJob :execrows
UPDATE jobs SET status = 'stopped', stop_reason = $2, finished_at = now(), updated_at = now()
WHERE id = $1 AND status = 'queued';

-- name: CountActiveJobsForRunner :one
SELECT count(*) FROM jobs WHERE runner_id = $1 AND status IN ('leased', 'running', 'stalled');

-- name: InsertReceipt :exec
INSERT INTO receipts (source, subject_kind, subject_id, status, payload, verified_at)
VALUES ($1, $2, $3, $4, $5, now());
