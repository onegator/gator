-- name: CreateRemoteAgent :one
INSERT INTO agents (project_id, handle, display_name, description, tier, kind, remote, remote_secrets, source, created_by)
VALUES (sqlc.arg(project_id), sqlc.arg(handle), sqlc.arg(display_name), sqlc.arg(description), 'chat', 'remote',
        sqlc.arg(remote), sqlc.arg(remote_secrets), '{"kind":"custom"}', sqlc.narg(created_by))
RETURNING *;

-- name: SetAgentExposed :one
UPDATE agents SET a2a_exposed = sqlc.arg(exposed), updated_at = now()
WHERE id = sqlc.arg(id) AND kind = 'local'
RETURNING *;

-- name: GetExposedAgent :one
-- An agent a project shows to its A2A clients, by the project's slug and the agent's handle.
SELECT a.* FROM agents a JOIN projects p ON p.id = a.project_id
WHERE p.slug = sqlc.arg(slug) AND a.handle = sqlc.arg(handle) AND a.a2a_exposed AND a.kind = 'local' AND a.deleted_at IS NULL;

-- name: CreateA2AClient :one
INSERT INTO a2a_clients (project_id, name, token_id, identity_id, daily_budget_usd, per_minute, created_by)
VALUES (sqlc.arg(project_id), sqlc.arg(name), sqlc.arg(token_id), sqlc.arg(identity_id), sqlc.arg(daily_budget_usd),
        sqlc.arg(per_minute), sqlc.narg(created_by))
RETURNING *;

-- name: GetA2AClient :one
SELECT * FROM a2a_clients WHERE id = $1;

-- name: GetA2AClientByToken :one
SELECT * FROM a2a_clients WHERE token_id = $1 AND revoked_at IS NULL;

-- name: ListA2AClients :many
SELECT * FROM a2a_clients WHERE project_id = $1 ORDER BY revoked_at IS NOT NULL, name;

-- name: RevokeA2AClient :one
UPDATE a2a_clients SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1 RETURNING *;

-- name: ExternalCostSince :one
-- What somebody outside asked for spent since a moment, or may still spend: the turns they asked for
-- and every turn agents delegated from those, finished jobs as they cost, jobs queued and running
-- at their cap.
WITH RECURSIVE asked AS (
    -- Turns asked for since a day before: a job started yesterday may still spend today.
    SELECT t.id FROM agent_turns t WHERE t.requested_by_kind = 'external' AND t.requested_by = sqlc.arg(identity_id)
      AND t.created_at >= sqlc.arg(since)::timestamptz - interval '1 day'
    UNION
    SELECT c.id FROM agent_turns c JOIN asked a ON c.parent_turn_id = a.id
)
SELECT (COALESCE((SELECT sum(u.cost_usd) FROM usage_records u JOIN jobs j ON j.id = u.job_id
                  WHERE j.turn_id IN (SELECT id FROM asked) AND u.created_at >= sqlc.arg(since)), 0)
      + COALESCE((SELECT sum(COALESCE((j.bounds->>'max_cost_usd')::float8, 0)) FROM jobs j
                  WHERE j.turn_id IN (SELECT id FROM asked) AND j.status IN ('queued', 'leased', 'running', 'stalled', 'settling')), 0))::float8;

-- name: CreateA2ATask :one
INSERT INTO a2a_tasks (client_id, project_id, agent_id, message_id, context_id)
VALUES (sqlc.arg(client_id), sqlc.arg(project_id), sqlc.arg(agent_id), sqlc.arg(message_id), sqlc.arg(context_id))
ON CONFLICT (message_id, agent_id) DO UPDATE SET context_id = a2a_tasks.context_id
RETURNING *;

-- name: GetA2ATask :one
SELECT * FROM a2a_tasks WHERE id = $1;

-- name: InsertA2ACall :exec
INSERT INTO a2a_calls (turn_id, agent_id, project_id) VALUES (sqlc.arg(turn_id), sqlc.arg(agent_id), sqlc.arg(project_id))
ON CONFLICT (turn_id) DO NOTHING;

-- name: ClaimA2ACall :one
-- The oldest call nobody is making: pending, or claimed by a worker that died (claim_ttl ago), of an
-- agent with fewer than per_agent calls being made right now.
UPDATE a2a_calls SET status = 'calling', claimed_at = now(), attempts = attempts + 1
WHERE turn_id = (SELECT c.turn_id FROM a2a_calls c
                 WHERE (c.status = 'pending' OR (c.status = 'calling' AND c.claimed_at < now() - sqlc.arg(claim_ttl)::interval))
                   AND (SELECT count(*) FROM a2a_calls b WHERE b.agent_id = c.agent_id AND b.status = 'calling'
                          AND b.claimed_at >= now() - sqlc.arg(claim_ttl)::interval) < sqlc.arg(per_agent)::bigint
                 ORDER BY c.created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: CountA2ACallsSince :one
SELECT count(*) FROM a2a_calls WHERE agent_id = sqlc.arg(agent_id) AND created_at >= sqlc.arg(since);

-- name: EndOpenA2ACall :execrows
-- Ends a call nobody has ended yet (a person stopped its turn).
UPDATE a2a_calls SET status = 'failed', error = sqlc.arg(error), finished_at = now(), claimed_at = NULL
WHERE turn_id = sqlc.arg(turn_id) AND status IN ('pending', 'calling');

-- name: SetA2ARemoteTask :exec
UPDATE a2a_calls SET remote_task_id = sqlc.arg(remote_task_id), claimed_at = now() WHERE turn_id = sqlc.arg(turn_id);

-- name: EndA2ACall :exec
UPDATE a2a_calls SET status = sqlc.arg(status), error = sqlc.arg(error), finished_at = now(), claimed_at = NULL
WHERE turn_id = sqlc.arg(turn_id);

-- name: GetA2ACall :one
SELECT * FROM a2a_calls WHERE turn_id = $1;

-- name: GetA2AClientByIdentity :one
SELECT * FROM a2a_clients WHERE identity_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC LIMIT 1;

-- name: CountOpenA2ATasks :one
-- A client's asks still being answered (or not read by the agents yet), of the last hour: what it
-- may spend that its budget does not count yet.
SELECT count(*) FROM a2a_tasks k
LEFT JOIN agent_turns t ON t.agent_id = k.agent_id AND t.trigger_message_id = k.message_id
WHERE k.client_id = sqlc.arg(client_id) AND k.created_at > now() - interval '1 hour'
  AND (t.id IS NULL OR t.status IN ('queued', 'running'));
