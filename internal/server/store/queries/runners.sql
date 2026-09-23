-- name: UpsertRunner :one
INSERT INTO runners (token_id, name, location, status, capabilities, protocol_version, binary_version, connected_at, last_heartbeat_at)
VALUES ($1, $2, $3, 'online', $4, $5, $6, now(), now())
ON CONFLICT (token_id) DO UPDATE
SET name = EXCLUDED.name, location = EXCLUDED.location, status = 'online',
    capabilities = EXCLUDED.capabilities, protocol_version = EXCLUDED.protocol_version,
    binary_version = EXCLUDED.binary_version, connected_at = now(), last_heartbeat_at = now(), updated_at = now(),
    offline_reason = '', offline_since = NULL
RETURNING *;

-- name: RunnerHeartbeat :exec
UPDATE runners SET last_heartbeat_at = now(), auth_state = $2, status = $3, updated_at = now() WHERE id = $1;

-- name: SetRunnerStatus :exec
UPDATE runners SET status = sqlc.arg(status), updated_at = now(),
    offline_reason = CASE WHEN sqlc.arg(status)::text = 'offline' THEN sqlc.arg(reason)::text ELSE '' END,
    offline_since = CASE WHEN sqlc.arg(status)::text = 'offline' THEN now() ELSE NULL END
WHERE id = sqlc.arg(id);

-- name: MarkSilentRunnersOffline :many
UPDATE runners SET status = 'offline', updated_at = now(),
    offline_reason = 'no heartbeat', offline_since = now()
WHERE status <> 'offline' AND last_heartbeat_at < sqlc.arg(before)
RETURNING id, name;

-- name: ListRunners :many
SELECT * FROM runners ORDER BY name;

-- name: GetRunner :one
SELECT * FROM runners WHERE id = $1;

-- name: CreateJob :one
INSERT INTO jobs (task_id, project_id, phase, role, backend, instruction, bounds, max_attempts, created_by_kind, created_by, model)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
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
    attempts = attempts + 1, updated_at = now(), unassignable_since = NULL
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

-- name: MarkUnassignableJobs :many
-- A queued job no connected runner can take. Matching mirrors LeaseJobs and the session's own
-- filter: the backend must be offered, the project allowed, and the backend's login must work,
-- because a runner with an expired login is online and still takes nothing. Jobs already marked
-- are skipped, so the event fires once per episode rather than on every sweep.
UPDATE jobs SET unassignable_since = now(), updated_at = now()
WHERE id IN (
    SELECT j.id FROM jobs j
    WHERE j.status = 'queued' AND j.unassignable_since IS NULL AND j.created_at < sqlc.arg(before)
      AND NOT EXISTS (
          SELECT 1 FROM runners r
          WHERE r.status <> 'offline'
            AND r.capabilities->'backends' @> to_jsonb(j.backend)
            AND (COALESCE(jsonb_array_length(r.capabilities->'projects'), 0) = 0
                 OR r.capabilities->'projects' @> to_jsonb(j.project_id::text))
            AND COALESCE(r.auth_state->>j.backend, '') NOT IN ('expired', 'missing')
      )
    FOR UPDATE SKIP LOCKED
)
RETURNING id, task_id, project_id, backend;

-- name: ListUnassignableTaskIDs :many
-- Tasks whose current phase has a job nobody can take, for the inbox.
SELECT DISTINCT j.task_id FROM jobs j
JOIN tasks t ON t.id = j.task_id AND t.phase = j.phase
WHERE j.status = 'queued' AND j.unassignable_since IS NOT NULL AND t.closed_at IS NULL;

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

-- name: LockTask :exec
-- Serialises job creation per task inside a transaction.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(key)::text, 0));

-- name: ListActiveJobsOnRunners :many
-- What each runner is doing now, so a person can stop or steer it from wherever they are —
-- including a phone, which has no task detail open.
SELECT j.id, j.runner_id, j.task_id, j.phase, j.role, j.backend, j.status, j.started_at, t.title AS task_title
FROM jobs j JOIN tasks t ON t.id = j.task_id
WHERE j.runner_id IS NOT NULL AND j.status IN ('leased', 'running', 'stalled')
ORDER BY j.started_at NULLS LAST;

-- name: RecordAgentCall :exec
INSERT INTO agent_calls (job_id, task_id, command, detail) VALUES ($1, $2, $3, $4);

-- name: ListAgentCalls :many
SELECT * FROM agent_calls WHERE job_id = $1 ORDER BY id LIMIT $2;
