-- name: GetAgent :one
SELECT * FROM agents WHERE id = $1;

-- name: ListAgentsVisibleIn :many
-- The agents a project's people can mention: the project's own and the workspace's.
SELECT * FROM agents WHERE project_id = sqlc.arg(project_id) OR project_id IS NULL
ORDER BY handle;

-- name: ListWorkspaceAgents :many
SELECT * FROM agents WHERE project_id IS NULL ORDER BY handle;

-- name: GetAgentByHandle :one
-- A handle names at most one agent where it is seen (see agents_handle_visible_once).
SELECT * FROM agents
WHERE handle = sqlc.arg(handle) AND (project_id = sqlc.arg(project_id) OR project_id IS NULL)
LIMIT 1;

-- name: AgentForRole :one
-- Who plays a role in a project: the project's own agent before the workspace's, and one a person
-- made for the role (a @backend-reviewer) before the built-in one. The template keeps saying
-- role: reviewer; a project swaps the agent without touching it.
SELECT * FROM agents
WHERE role_key = sqlc.arg(role_key) AND (project_id = sqlc.arg(project_id) OR project_id IS NULL)
ORDER BY project_id IS NULL, (source->>'kind' = 'builtin'), created_at DESC
LIMIT 1;
