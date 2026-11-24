-- name: GetAgent :one
SELECT * FROM agents WHERE id = $1;

-- name: ListAgentsVisibleIn :many
-- The agents a project's people can mention: the project's own and the workspace's.
SELECT * FROM agents WHERE (project_id = sqlc.arg(project_id) OR project_id IS NULL) AND deleted_at IS NULL
ORDER BY handle;

-- name: ListWorkspaceAgents :many
SELECT * FROM agents WHERE project_id IS NULL AND deleted_at IS NULL ORDER BY handle;

-- name: GetAgentByHandle :one
-- A handle names at most one agent where it is seen (see agents_handle_visible_once).
SELECT * FROM agents
WHERE handle = sqlc.arg(handle) AND (project_id = sqlc.arg(project_id) OR project_id IS NULL) AND deleted_at IS NULL
LIMIT 1;

-- name: AgentForRole :one
-- Who plays a role in a project: the project's own agent before the workspace's, and one a person
-- made for the role (a @backend-reviewer) before the built-in one. The template keeps saying
-- role: reviewer; a project swaps the agent without touching it.
SELECT * FROM agents
WHERE role_key = sqlc.arg(role_key) AND (project_id = sqlc.arg(project_id) OR project_id IS NULL) AND deleted_at IS NULL
ORDER BY project_id IS NULL, (source->>'kind' = 'builtin'), created_at DESC
LIMIT 1;

-- name: InsertTurn :one
-- One turn per agent and message that asked it: a redelivered mention finds the first (no row).
INSERT INTO agent_turns (agent_id, project_id, conversation_id, trigger_message_id, requested_by_kind, requested_by,
                         on_behalf_of, chain_id, parent_turn_id, escalated_from, hop, tier, status, refusal)
VALUES (sqlc.arg(agent_id), sqlc.arg(project_id), sqlc.arg(conversation_id), sqlc.narg(trigger_message_id),
        sqlc.arg(requested_by_kind), sqlc.narg(requested_by), sqlc.narg(on_behalf_of),
        COALESCE(sqlc.narg(chain_id)::uuid, gen_random_uuid()),
        sqlc.narg(parent_turn_id), sqlc.narg(escalated_from), sqlc.arg(hop), sqlc.arg(tier), sqlc.arg(status), sqlc.narg(refusal))
ON CONFLICT (agent_id, trigger_message_id) DO NOTHING
RETURNING *;

-- name: GetTurn :one
SELECT * FROM agent_turns WHERE id = $1;

-- name: GetTurnForUpdate :one
SELECT * FROM agent_turns WHERE id = $1 FOR UPDATE;

-- name: GetTurnByTrigger :one
SELECT * FROM agent_turns WHERE agent_id = sqlc.arg(agent_id) AND trigger_message_id = sqlc.arg(trigger_message_id);

-- name: GetTurnByJob :one
SELECT * FROM agent_turns WHERE job_id = $1;

-- name: SetTurnReply :one
UPDATE agent_turns SET reply_message_id = sqlc.arg(reply_message_id), updated_at = now()
WHERE id = sqlc.arg(id) RETURNING *;

-- name: SetTurnJob :one
UPDATE agent_turns SET job_id = sqlc.arg(job_id), tier = sqlc.arg(tier), updated_at = now()
WHERE id = sqlc.arg(id) RETURNING *;

-- name: SetTurnStatus :one
-- A turn moves on until it ends; an ended turn stays as it ended.
UPDATE agent_turns
SET status = sqlc.arg(status), refusal = sqlc.narg(refusal), updated_at = now(),
    finished_at = CASE WHEN sqlc.arg(status)::text IN ('done', 'failed', 'stopped', 'refused') THEN now() ELSE finished_at END
WHERE id = sqlc.arg(id) AND status IN ('queued', 'running')
RETURNING *;

-- name: ChainCost :one
-- What a chain of delegations has spent, or may still spend: the cost of every job of every turn
-- in it (and of the phase job a chain started from, whose id is the chain's), finished jobs as
-- they cost, jobs still queued or running at the cap they may spend.
SELECT (COALESCE((SELECT sum(u.cost_usd) FROM usage_records u JOIN jobs j ON j.id = u.job_id
                  LEFT JOIN agent_turns t ON t.id = j.turn_id
                  WHERE t.chain_id = sqlc.arg(chain_id) OR j.id = sqlc.arg(chain_id)), 0)
      + COALESCE((SELECT sum(COALESCE((j.bounds->>'max_cost_usd')::float8, 0)) FROM jobs j
                  LEFT JOIN agent_turns t ON t.id = j.turn_id
                  WHERE (t.chain_id = sqlc.arg(chain_id) OR j.id = sqlc.arg(chain_id))
                    AND j.status IN ('queued', 'leased', 'running', 'stalled')), 0))::float8;

-- name: ChainTurns :one
-- How many turns a chain has had, and how many the asking turn (or the phase job the chain
-- started from) has asked for, refusals not counted.
SELECT count(*)::int AS turns,
       (count(*) FILTER (WHERE parent_turn_id = sqlc.narg(parent_id)::uuid
                            OR (sqlc.narg(parent_id)::uuid IS NULL AND hop = 1)))::int AS asked_by_parent
FROM agent_turns
WHERE chain_id = sqlc.arg(chain_id) AND status <> 'refused';

-- name: AgentCostSince :one
-- What an agent's jobs spent since a moment, or may still spend (queued and running at their cap).
SELECT (COALESCE((SELECT sum(u.cost_usd) FROM usage_records u JOIN jobs j ON j.id = u.job_id
                  WHERE j.agent_id = sqlc.arg(agent_id) AND u.created_at >= sqlc.arg(since)), 0)
      + COALESCE((SELECT sum(COALESCE((j.bounds->>'max_cost_usd')::float8, 0)) FROM jobs j
                  WHERE j.agent_id = sqlc.arg(agent_id) AND j.status IN ('queued', 'leased', 'running', 'stalled')), 0))::float8;

-- name: ListActiveTurnsIn :many
-- The turns still answering in a conversation, oldest first.
SELECT * FROM agent_turns WHERE conversation_id = $1 AND status IN ('queued', 'running') ORDER BY created_at;

-- name: ListConversationAgentIDs :many
SELECT member_id FROM conversation_members WHERE conversation_id = $1 AND member_kind = 'agent';

-- name: ListTurnsOfReplies :many
-- What is said under an agent's answer: its turn, and what its job cost and took.
SELECT t.id, t.reply_message_id, t.agent_id, t.status, t.tier, t.job_id, t.refusal,
       COALESCE(u.cost_usd, 0)::float8 AS cost_usd, COALESCE(u.duration_ms, 0)::bigint AS duration_ms,
       COALESCE(u.estimated, false)::boolean AS cost_estimated
FROM agent_turns t
LEFT JOIN LATERAL (
    SELECT sum(r.cost_usd) AS cost_usd, sum(r.duration_ms) AS duration_ms, bool_or(r.cost_estimated) AS estimated
    FROM usage_records r WHERE r.job_id = t.job_id
) u ON true
WHERE t.reply_message_id = ANY(sqlc.arg(ids)::uuid[]);

-- name: JobProgress :one
-- How far an agent's job has got, from its events: how many tools it called and the last one.
SELECT count(*) FILTER (WHERE type = 'tool_call')::int AS tool_calls,
       COALESCE((SELECT e2.payload->>'name' FROM job_events e2 WHERE e2.job_id = $1 AND e2.type = 'tool_call'
                 ORDER BY e2.seq DESC LIMIT 1), '')::text AS last_tool
FROM job_events WHERE job_id = $1;

-- name: JobCost :one
SELECT COALESCE(sum(cost_usd), 0)::float8 AS cost_usd, COALESCE(sum(duration_ms), 0)::bigint AS duration_ms
FROM usage_records WHERE job_id = $1;

-- name: CreateAgent :one
INSERT INTO agents (project_id, handle, display_name, description, avatar, role_key, instructions, tier, policy, status, source, created_by)
VALUES (sqlc.narg(project_id), sqlc.arg(handle), sqlc.arg(display_name), sqlc.arg(description), sqlc.arg(avatar), sqlc.narg(role_key),
        sqlc.arg(instructions), sqlc.arg(tier), sqlc.arg(policy), sqlc.arg(status), '{"kind":"custom"}', sqlc.narg(created_by))
RETURNING *;

-- name: UpdateAgent :one
UPDATE agents
SET handle = sqlc.arg(handle), display_name = sqlc.arg(display_name), description = sqlc.arg(description),
    avatar = sqlc.arg(avatar), role_key = sqlc.narg(role_key), instructions = sqlc.arg(instructions), tier = sqlc.arg(tier),
    policy = sqlc.arg(policy), status = sqlc.arg(status), updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteAgent :execrows
-- Puts an agent people made away; its turns stay, and so does what they cost.
UPDATE agents SET deleted_at = now(), status = 'paused', updated_at = now()
WHERE id = $1 AND source->>'kind' <> 'builtin' AND deleted_at IS NULL;

-- name: CountActiveTurnsOfAgent :one
SELECT count(*)::int FROM agent_turns WHERE agent_id = $1 AND status IN ('queued', 'running');

-- name: AgentStats :one
-- An agent's record since a moment: its turns, its jobs (phase work and answers), how many of
-- those that ended ended done, and what they cost.
SELECT
    (SELECT count(*) FROM agent_turns t WHERE t.agent_id = sqlc.arg(agent_id) AND t.created_at >= sqlc.arg(since))::int AS turns,
    (SELECT count(*) FROM agent_turns t WHERE t.agent_id = sqlc.arg(agent_id) AND t.created_at >= sqlc.arg(since) AND t.status = 'refused')::int AS refused,
    (SELECT count(*) FROM jobs j WHERE j.agent_id = sqlc.arg(agent_id) AND j.created_at >= sqlc.arg(since))::int AS jobs,
    (SELECT count(*) FROM jobs j WHERE j.agent_id = sqlc.arg(agent_id) AND j.created_at >= sqlc.arg(since) AND j.status = 'done')::int AS done,
    (SELECT count(*) FROM jobs j WHERE j.agent_id = sqlc.arg(agent_id) AND j.created_at >= sqlc.arg(since)
        AND j.status IN ('done', 'failed', 'stopped'))::int AS finished,
    COALESCE((SELECT sum(u.cost_usd) FROM usage_records u JOIN jobs j ON j.id = u.job_id
              WHERE j.agent_id = sqlc.arg(agent_id) AND u.created_at >= sqlc.arg(since)), 0)::float8 AS cost_usd;

-- name: ListAgentTurns :many
SELECT * FROM agent_turns
WHERE agent_id = sqlc.arg(agent_id) AND created_at >= sqlc.arg(since)
ORDER BY created_at DESC
LIMIT sqlc.arg(max_rows);

-- name: ClaimTurn :one
-- Claims a turn that has no job yet for whoever starts it: one claimer at a time, and a claim
-- nobody finished (a crash) expires after claim_ttl. No row: somebody else is starting it, or it
-- has its job, or it ended.
UPDATE agent_turns SET claimed_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'queued' AND job_id IS NULL
  AND (claimed_at IS NULL OR claimed_at < now() - sqlc.arg(claim_ttl)::interval)
RETURNING *;

-- name: ReleaseTurn :exec
UPDATE agent_turns SET claimed_at = NULL WHERE id = $1 AND job_id IS NULL;

-- name: EndClaimedTurn :one
-- Ends a turn its claimer holds (a refusal found on redelivery, a stop before any job).
UPDATE agent_turns
SET status = sqlc.arg(status), refusal = sqlc.narg(refusal), claimed_at = NULL, finished_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'queued' AND job_id IS NULL
RETURNING *;

-- name: GetJobOfTurn :one
-- The job a turn's start queued, even when the start did not live to record it on the turn.
SELECT * FROM jobs WHERE turn_id = $1 AND kind = 'reply' ORDER BY created_at LIMIT 1;

-- name: ListStaleTurns :many
-- Turns that never got a job, whose start nobody holds any more.
SELECT * FROM agent_turns
WHERE status = 'queued' AND job_id IS NULL AND created_at < sqlc.arg(before)
  AND (claimed_at IS NULL OR claimed_at < sqlc.arg(before))
ORDER BY created_at DESC LIMIT 500;

-- name: ListWaitingTurns :many
-- Turns whose job no runner has taken for a while, to say so under the answer.
SELECT t.* FROM agent_turns t JOIN jobs j ON j.id = t.job_id
WHERE t.status = 'queued' AND j.status = 'queued' AND j.created_at < sqlc.arg(before)
ORDER BY t.created_at DESC LIMIT 500;
