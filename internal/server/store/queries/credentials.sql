-- name: ListUserCredentials :many
SELECT * FROM user_credentials WHERE user_id = $1 ORDER BY created_at, kind;

-- name: GetUserCredential :one
SELECT * FROM user_credentials WHERE user_id = $1 AND kind = $2;

-- name: GetUserCredentialByID :one
SELECT * FROM user_credentials WHERE id = $1;

-- name: UpsertUserCredential :one
-- Saving a login again replaces it whole: a new secret starts unchecked, with no error.
INSERT INTO user_credentials (user_id, kind, backend, secret, hint)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, kind) DO UPDATE
SET backend = EXCLUDED.backend, secret = EXCLUDED.secret, hint = EXCLUDED.hint,
    status = 'unchecked', last_error = '', checked_at = NULL, updated_at = now(),
    version = user_credentials.version + 1
RETURNING *;

-- name: DeleteUserCredential :execrows
DELETE FROM user_credentials WHERE user_id = $1 AND kind = $2;

-- name: SetUserCredentialStatus :one
UPDATE user_credentials
SET status = $2, last_error = $3, checked_at = COALESCE(sqlc.narg(checked_at), checked_at), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ReplaceUserCredentialSecret :execrows
-- What the agent rewrote while it worked (a refreshed token); the login keeps its standing. Only
-- the version the job was leased: a login its owner saved again since is theirs, not the job's.
UPDATE user_credentials SET secret = $2, hint = $3, updated_at = now(), version = version + 1
WHERE id = $1 AND version = sqlc.arg(leased_version);

-- name: TouchUserCredential :exec
UPDATE user_credentials SET last_used_at = now() WHERE id = $1;

-- name: CredentialHeld :one
-- Whether another active job holds a login now.
SELECT EXISTS (
    SELECT 1 FROM jobs
    WHERE credential_id = sqlc.arg(credential_id) AND id <> sqlc.arg(job_id)
      AND status IN ('leased', 'running', 'stalled')
)::boolean AS held;

-- name: ReleaseWaitingJobs :execrows
-- A person connected a login: their queued jobs stop saying they wait for one, and the next
-- lease decides again.
UPDATE jobs SET waiting_reason = '', updated_at = now()
WHERE payer_id = $1 AND status = 'queued' AND waiting_reason <> '';

-- name: ReleaseAllWaitingJobs :execrows
UPDATE jobs SET waiting_reason = '', updated_at = now()
WHERE status = 'queued' AND waiting_reason <> '';

-- name: SetJobWaiting :exec
UPDATE jobs SET waiting_reason = $2, updated_at = now() WHERE id = $1 AND status = 'queued';

-- name: SetRunnerLogins :one
UPDATE workspace_settings SET runner_logins = $1, updated_at = now() WHERE id RETURNING *;

-- name: UsageByCredential :many
-- What a person's own logins paid for since a moment, by kind.
SELECT credential_kind,
       count(DISTINCT job_id)::bigint AS jobs,
       COALESCE(sum(input_tokens), 0)::bigint AS input_tokens,
       COALESCE(sum(output_tokens), 0)::bigint AS output_tokens,
       COALESCE(sum(cache_read_tokens), 0)::bigint AS cache_read_tokens,
       COALESCE(sum(cache_write_tokens), 0)::bigint AS cache_write_tokens,
       COALESCE(sum(cost_usd), 0)::double precision AS cost_usd,
       COALESCE(sum(duration_ms), 0)::bigint AS duration_ms
FROM usage_records
WHERE paid_by = sqlc.arg(user_id) AND created_at >= sqlc.arg(since)
GROUP BY credential_kind
ORDER BY credential_kind;

-- name: UsageByPayer :many
-- What every login paid for since a moment, by person and kind; a job on the runner's own login
-- has no person.
SELECT u.paid_by, COALESCE(p.name, '')::text AS name, COALESCE(p.email, '')::text AS email, u.credential_kind,
       count(DISTINCT u.job_id)::bigint AS jobs,
       COALESCE(sum(u.input_tokens), 0)::bigint AS input_tokens,
       COALESCE(sum(u.output_tokens), 0)::bigint AS output_tokens,
       COALESCE(sum(u.cache_read_tokens), 0)::bigint AS cache_read_tokens,
       COALESCE(sum(u.cache_write_tokens), 0)::bigint AS cache_write_tokens,
       COALESCE(sum(u.cost_usd), 0)::double precision AS cost_usd,
       COALESCE(sum(u.duration_ms), 0)::bigint AS duration_ms
FROM usage_records u
LEFT JOIN users p ON p.id = u.paid_by
WHERE u.created_at >= sqlc.arg(since) AND u.credential_kind <> ''
GROUP BY u.paid_by, p.name, p.email, u.credential_kind
ORDER BY p.name, u.credential_kind;

-- name: TaskCreator :one
-- The person who created a task, from its history; no row when nobody did (a plugin, the system).
SELECT actor_id FROM phase_transitions
WHERE task_id = $1 AND kind = 'create' AND actor_kind = 'user' AND actor_id IS NOT NULL
ORDER BY created_at LIMIT 1;

-- name: RunnerLoginsAllowed :one
SELECT runner_logins FROM workspace_settings WHERE id;

-- name: ReleaseFreedWaits :execrows
-- Queued jobs that wait while their payer has a usable login nobody holds — a ChatGPT login whose
-- job ended, was stopped or lost its lease — stop saying they wait; the next lease decides again.
UPDATE jobs j SET waiting_reason = '', updated_at = now()
WHERE j.status = 'queued' AND j.waiting_reason <> '' AND j.payer_id IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM user_credentials c
      WHERE c.user_id = j.payer_id AND c.backend = j.backend AND c.status <> 'error'
        AND NOT EXISTS (SELECT 1 FROM jobs a WHERE a.credential_id = c.id AND a.credential_exclusive
                        AND a.status IN ('leased', 'running', 'stalled')));
