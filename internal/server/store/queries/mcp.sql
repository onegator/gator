-- name: CreateMCPServer :one
INSERT INTO mcp_servers (project_id, name, display_name, url, auth, allowed_tools, read_only_tools, oauth, source)
VALUES (sqlc.narg(project_id), sqlc.arg(name), sqlc.arg(display_name), sqlc.arg(url), sqlc.arg(auth), sqlc.arg(allowed_tools),
        sqlc.arg(read_only_tools), sqlc.arg(oauth), sqlc.arg(source))
RETURNING *;

-- name: GetMCPServer :one
SELECT * FROM mcp_servers WHERE id = $1;

-- name: GetMCPServerIn :one
-- A scope's own server of a name: a project's (project set) or the workspace's (NULL).
SELECT * FROM mcp_servers WHERE project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::uuid AND name = sqlc.arg(name);

-- name: MCPServerFor :one
-- The server of a name a project's agents reach: the project's own before the workspace's.
SELECT * FROM mcp_servers WHERE name = sqlc.arg(name) AND (project_id = sqlc.arg(project_id) OR project_id IS NULL)
ORDER BY project_id IS NULL LIMIT 1;

-- name: ListMCPServersVisibleIn :many
-- A project's servers and the workspace's it does not shadow.
SELECT DISTINCT ON (name) * FROM mcp_servers
WHERE project_id = sqlc.arg(project_id) OR project_id IS NULL
ORDER BY name, project_id IS NULL;

-- name: UpdateMCPServer :one
-- A new address drops, in the same statement, every credential the old one had (and what OAuth
-- discovery found for it): no moment in which the old token could go to the new host.
UPDATE mcp_servers
SET url = sqlc.arg(url), allowed_tools = sqlc.arg(allowed_tools), read_only_tools = sqlc.arg(read_only_tools), enabled = sqlc.arg(enabled),
    secrets = CASE WHEN url = sqlc.arg(url) THEN secrets ELSE '' END,
    token_expires_at = CASE WHEN url = sqlc.arg(url) THEN token_expires_at END,
    oauth = CASE WHEN url = sqlc.arg(url) THEN oauth ELSE oauth - 'discovered' - 'client' END,
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: GetMCPServerForUpdate :one
SELECT * FROM mcp_servers WHERE project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::uuid AND name = sqlc.arg(name) FOR UPDATE;

-- name: DeleteMCPConnectStates :exec
-- Connections of a server under way, when what they were started for changed.
DELETE FROM plugin_connect_states WHERE target = 'mcp' AND project_id = sqlc.arg(project_id) AND plugin_name = sqlc.arg(name);

-- name: SetMCPServerSecrets :one
UPDATE mcp_servers SET secrets = sqlc.arg(secrets), token_expires_at = sqlc.narg(token_expires_at), updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteMCPServer :execrows
DELETE FROM mcp_servers WHERE id = $1;

-- name: InsertMCPCall :exec
INSERT INTO mcp_calls (mcp_server_id, server_name, project_id, job_id, agent_id, method, tool, payload, outcome, error, result_bytes, duration_ms)
VALUES (sqlc.arg(mcp_server_id), sqlc.arg(server_name), sqlc.narg(project_id), sqlc.narg(job_id), sqlc.narg(agent_id), sqlc.arg(method), sqlc.arg(tool),
        sqlc.arg(payload), sqlc.arg(outcome), sqlc.narg(error), sqlc.arg(result_bytes), sqlc.arg(duration_ms));

-- name: ListMCPCalls :many
-- A server's calls from one project, newest first.
SELECT * FROM mcp_calls WHERE mcp_server_id = sqlc.arg(mcp_server_id) AND project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::uuid
ORDER BY id DESC LIMIT sqlc.arg(max_rows);

-- name: ListMCPServerNamesVisibleIn :many
SELECT DISTINCT name FROM mcp_servers WHERE (project_id = sqlc.arg(project_id) OR project_id IS NULL) AND enabled;

-- name: ListWorkspaceMCPServers :many
SELECT * FROM mcp_servers WHERE project_id IS NULL ORDER BY name;

-- name: SetMCPServerOAuth :one
UPDATE mcp_servers SET oauth = sqlc.arg(oauth), updated_at = now() WHERE id = sqlc.arg(id) RETURNING *;

-- name: ListMCPCallsOfServer :many
-- A workspace server's calls from every project, newest first, for workspace admins.
SELECT * FROM mcp_calls WHERE mcp_server_id = sqlc.arg(mcp_server_id) ORDER BY id DESC LIMIT sqlc.arg(max_rows);
