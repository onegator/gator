-- name: UpsertRunner :one
-- The owner is whoever minted the token; a token minted on the host has none.
INSERT INTO runners (token_id, name, location, status, capabilities, protocol_version, binary_version, connected_at, last_heartbeat_at,
                     placement, owner_id)
VALUES ($1, $2, $3, 'online', $4, $5, $6, now(), now(),
        sqlc.arg(placement), (SELECT t.user_id FROM api_tokens t WHERE t.id = $1))
ON CONFLICT (token_id) DO UPDATE
SET name = EXCLUDED.name, location = EXCLUDED.location, status = 'online', placement = EXCLUDED.placement,
    owner_id = EXCLUDED.owner_id,
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
SELECT * FROM runners WHERE retired_at IS NULL ORDER BY name;

-- name: RetireRunner :one
UPDATE runners SET retired_at = now(), status = 'offline', updated_at = now(),
    offline_reason = 'forgotten', offline_since = now()
WHERE id = $1 AND retired_at IS NULL
RETURNING *;

-- name: GetRunner :one
SELECT * FROM runners WHERE id = $1;

-- name: CreateJob :one
INSERT INTO jobs (task_id, project_id, phase, role, backend, instruction, bounds, max_attempts, created_by_kind, created_by, model, placement,
                  agent_id, turn_id, tier, kind, payer_id, waiting_reason)
-- An empty placement is local, which is what every job was before it had one; an empty tier is
-- work and an empty kind a phase job, which is what every job was before agents had turns.
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, COALESCE(NULLIF(sqlc.arg(placement)::text, ''), 'local'),
        sqlc.narg(agent_id), sqlc.narg(turn_id), COALESCE(NULLIF(sqlc.arg(tier)::text, ''), 'work'),
        COALESCE(NULLIF(sqlc.arg(kind)::text, ''), 'phase'), sqlc.narg(payer_id), sqlc.arg(waiting_reason))
RETURNING *;

-- name: TaskWorkBranch :one
-- The branch a task's latest finished work was pushed to: what an agent reading the task reads.
SELECT (receipt->>'branch')::text AS branch FROM jobs
WHERE task_id = $1 AND status = 'done' AND COALESCE(receipt->>'branch', '') <> ''
ORDER BY finished_at DESC NULLS LAST LIMIT 1;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = $1;

-- name: GetJobForUpdate :one
SELECT * FROM jobs WHERE id = $1 FOR UPDATE;

-- name: ListJobsByTask :many
SELECT * FROM jobs WHERE task_id = $1 ORDER BY created_at;

-- name: LeaseCandidates :many
-- The queued jobs a runner could be handed, oldest first, locked until the lease's transaction
-- ends. SKIP LOCKED lets concurrent runners consider disjoint jobs without waiting on each other.
-- Whose login each runs on is decided in Go (runners.Credentials); a job that waits for a login
-- goes after those that do not, so a queue of them cannot hold the rest back.
--
-- Placement must match. A local job someone started goes to a machine of theirs or to a shared
-- one (no owner), never to another person's Mac, which is logged in as somebody else. A job the
-- autopilot queued has no person behind it and takes any local runner. A runner started for one
-- job (only_job) takes that job and nothing else.
SELECT j.* FROM jobs j
WHERE j.status = 'queued'
  AND j.backend = ANY(sqlc.arg(backends)::text[])
  AND (cardinality(sqlc.arg(projects)::uuid[]) = 0 OR j.project_id = ANY(sqlc.arg(projects)::uuid[]))
  AND j.placement = sqlc.arg(placement)::text
  AND (sqlc.narg(only_job)::uuid IS NULL OR j.id = sqlc.narg(only_job)::uuid)
  AND (j.placement = 'cloud' OR j.created_by_kind <> 'user' OR j.created_by IS NULL
       OR sqlc.narg(owner)::uuid IS NULL OR j.created_by = sqlc.narg(owner)::uuid)
  -- A person's own login goes only to their own machine or a shared runner, whoever queued the
  -- job: the autopilot's work, a summary or an automation's job paid by Bob never reaches Ann's
  -- Mac, whose agent could read it. A payer with no login for the backend brings none.
  AND (sqlc.narg(owner)::uuid IS NULL OR j.payer_id IS NULL OR j.payer_id = sqlc.narg(owner)::uuid
       OR NOT EXISTS (SELECT 1 FROM user_credentials c WHERE c.user_id = j.payer_id AND c.backend = j.backend))
  -- A runner older than protocol v3 runs every job as a phase; it gets only those.
  AND (NOT sqlc.arg(phase_only)::boolean OR j.kind = 'phase')
  -- A chat job goes only to a backend that keeps it read-only, a summary to one that runs it
  -- with no tools: a runner would refuse it anywhere else.
  AND (j.tier <> 'chat' OR j.backend = ANY(sqlc.arg(read_only)::text[]))
  AND (j.kind <> 'summary' OR j.backend = ANY(sqlc.arg(no_tools)::text[]))
ORDER BY (j.waiting_reason <> ''), j.created_at
LIMIT sqlc.arg(slots)
FOR UPDATE OF j SKIP LOCKED;

-- name: LeaseJob :one
-- Hands one candidate to a runner, with the login it runs on.
UPDATE jobs
SET status = 'leased', runner_id = sqlc.arg(runner_id), lease_expires_at = sqlc.arg(lease_until),
    attempts = attempts + 1, updated_at = now(), unassignable_since = NULL, waiting_reason = '',
    credential_id = sqlc.narg(credential_id), credential_kind = sqlc.arg(credential_kind),
    credential_exclusive = sqlc.arg(credential_exclusive)
WHERE id = sqlc.arg(id) AND status = 'queued'
RETURNING *;

-- name: LockLeases :exec
-- Leases are handed out one transaction at a time, so a login one job at a time may hold is never
-- handed to two runners at once. A lease is a handful of rows; the wait is short.
SELECT pg_advisory_xact_lock(7413020250);

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
-- A queued job no connected runner can take. Matching mirrors LeaseCandidates and the session's own
-- filter: the backend must be offered, the project allowed, and the backend's login must work,
-- because a runner with an expired login is online and still takes nothing. Jobs already marked
-- are skipped, so the event fires once per episode rather than on every sweep. Placement and
-- ownership match as in LeaseCandidates. A cloud job a provisioner will start a runner for is left
-- alone (skip_cloud): nobody is missing, the runner is on its way.
UPDATE jobs SET unassignable_since = now(), updated_at = now()
WHERE id IN (
    SELECT j.id FROM jobs j
    WHERE j.status = 'queued' AND j.unassignable_since IS NULL AND j.created_at < sqlc.arg(before)
      -- A job waiting for its payer's login already says why it waits.
      AND j.waiting_reason = ''
      AND NOT (sqlc.arg(skip_cloud)::boolean AND j.placement = 'cloud')
      AND NOT EXISTS (
          SELECT 1 FROM runners r
          WHERE r.status <> 'offline'
            AND r.placement = j.placement
            AND (j.placement = 'cloud' OR j.created_by_kind <> 'user' OR j.created_by IS NULL
                 OR r.owner_id IS NULL OR r.owner_id = j.created_by)
            AND r.capabilities->'backends' @> to_jsonb(j.backend)
            -- A runner that sent "projects": null takes any project; jsonb_array_length of that
            -- null is an error, not zero, and used to fail the whole sweep.
            AND (COALESCE(jsonb_array_length(CASE WHEN jsonb_typeof(r.capabilities->'projects') = 'array'
                                                  THEN r.capabilities->'projects' END), 0) = 0
                 OR r.capabilities->'projects' @> to_jsonb(j.project_id::text))
            -- The runner's own login matters only to a job that does not bring its payer's.
            AND (COALESCE(r.auth_state->>j.backend, '') NOT IN ('expired', 'missing')
                 OR (r.capabilities->'payer_logins' @> to_jsonb(j.backend) AND j.payer_id IS NOT NULL
                     AND (r.owner_id IS NULL OR r.owner_id = j.payer_id) AND EXISTS (
                     SELECT 1 FROM user_credentials c
                     WHERE c.user_id = j.payer_id AND c.backend = j.backend AND c.status <> 'error')))
            AND (j.kind = 'phase' OR r.protocol_version >= 3)
      )
    FOR UPDATE SKIP LOCKED
)
RETURNING id, task_id, project_id, backend, placement;

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
SELECT j.id, j.runner_id, j.task_id, j.phase, j.role, j.backend, j.status, j.started_at, j.placement, t.title AS task_title
FROM jobs j JOIN tasks t ON t.id = j.task_id
WHERE j.runner_id IS NOT NULL AND j.status IN ('leased', 'running', 'stalled')
ORDER BY j.started_at NULLS LAST;

-- name: RecordAgentCall :exec
INSERT INTO agent_calls (job_id, task_id, command, detail) VALUES ($1, $2, $3, $4);

-- name: ListAgentCalls :many
SELECT * FROM agent_calls WHERE job_id = $1 ORDER BY id LIMIT $2;

-- name: SaveJobContextDoc :exec
INSERT INTO job_context (job_id, position, kind, phase, title, origin, body, full_bytes, left_out, dropped)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (job_id, position) DO UPDATE
SET kind = EXCLUDED.kind, phase = EXCLUDED.phase, title = EXCLUDED.title, origin = EXCLUDED.origin,
    body = EXCLUDED.body, full_bytes = EXCLUDED.full_bytes, left_out = EXCLUDED.left_out, dropped = EXCLUDED.dropped;

-- name: ListJobContext :many
SELECT * FROM job_context WHERE job_id = $1 ORDER BY position;

-- name: BumpJobObjections :one
UPDATE jobs SET objections = objections + 1, updated_at = now() WHERE id = $1 RETURNING objections;

-- name: ListQueuedCloudJobs :many
-- Cloud jobs waiting for a runner, oldest first: what a provisioner may start machines for.
SELECT * FROM jobs WHERE status = 'queued' AND placement = 'cloud'
  -- A job waiting for its payer's login gets no machine: it would sit there with nothing to run.
  AND waiting_reason = ''
ORDER BY created_at LIMIT sqlc.arg(max);

-- name: ActiveTrustDomains :many
-- Whose logins the runner's active jobs hold: a payer's id for a job on their own login, '' for a
-- job on the runner's. Jobs on one runner share a user, so they can read each other's logins.
SELECT DISTINCT (CASE WHEN credential_kind IN ('', 'runner') OR payer_id IS NULL THEN '' ELSE payer_id::text END)::text AS domain
FROM jobs WHERE runner_id = $1 AND status IN ('leased', 'running', 'stalled');
