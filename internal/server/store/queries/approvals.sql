-- name: InsertRuntimeRequest :one
-- A request sent again (a runner resends what was not acked) is the one already there.
INSERT INTO runtime_requests (id, job_id, turn_id, kind, tool, input, rule, tool_use_id, expires_at)
VALUES (sqlc.arg(id), sqlc.arg(job_id), sqlc.narg(turn_id), sqlc.arg(kind), sqlc.narg(tool), sqlc.arg(input), sqlc.arg(rule),
        sqlc.arg(tool_use_id), sqlc.arg(expires_at))
ON CONFLICT (id) DO NOTHING
RETURNING *;

-- name: GetRuntimeRequest :one
SELECT * FROM runtime_requests WHERE id = $1;

-- name: GetRuntimeRequestForUpdate :one
SELECT * FROM runtime_requests WHERE id = $1 FOR UPDATE;

-- name: DecideRuntimeRequest :one
-- The decision is taken only of a request still waiting: whoever comes second finds it decided.
UPDATE runtime_requests SET status = sqlc.arg(status), feedback = sqlc.narg(feedback), decided_by = sqlc.narg(decided_by),
    stopped = sqlc.arg(stopped), decided_at = now()
WHERE id = sqlc.arg(id) AND status = 'pending'
RETURNING *;

-- name: ExpireRuntimeRequests :many
-- Rows are taken in id order, as every writer of more than one request takes them.
UPDATE runtime_requests SET status = 'expired', decided_at = now()
WHERE id IN (SELECT r.id FROM runtime_requests r WHERE r.status = 'pending' AND r.expires_at <= sqlc.arg(now)
             ORDER BY r.id FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: CancelJobRuntimeRequests :many
UPDATE runtime_requests SET status = 'cancelled', decided_at = now()
WHERE job_id = $1 AND status = 'pending'
RETURNING *;

-- name: ListJobRuntimeRequests :many
SELECT * FROM runtime_requests WHERE job_id = $1 ORDER BY created_at;

-- name: ListPendingRuntimeRequestsOfJob :many
SELECT * FROM runtime_requests WHERE job_id = $1 AND status = 'pending' ORDER BY id FOR UPDATE;

-- name: ListPendingRuntimeRequests :many
-- What waits for a person, newest first, in the given projects (all when none are given).
SELECT r.* FROM runtime_requests r JOIN jobs j ON j.id = r.job_id
WHERE r.status = 'pending' AND (cardinality(sqlc.arg(projects)::uuid[]) = 0 OR j.project_id = ANY(sqlc.arg(projects)::uuid[]))
ORDER BY r.created_at DESC
LIMIT 200;

-- name: ListUserProjectIDs :many
SELECT project_id FROM memberships WHERE user_id = $1;

-- name: CancelRuntimeRequest :one
-- The runner no longer waits on it: the agent's CLI took the request back, or the turn ended.
UPDATE runtime_requests SET status = 'cancelled', decided_at = now()
WHERE id = $1 AND job_id = $2 AND status = 'pending'
RETURNING *;
