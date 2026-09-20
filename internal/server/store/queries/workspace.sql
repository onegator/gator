-- name: GetWorkspaceSettings :one
-- The row always exists (the migration seeds it), so callers do not have to handle its absence.
INSERT INTO workspace_settings (id) VALUES (true)
ON CONFLICT (id) DO UPDATE SET id = true
RETURNING *;

-- name: SetWorkspaceProcessConfig :one
INSERT INTO workspace_settings (id, process_config, updated_at) VALUES (true, $1, now())
ON CONFLICT (id) DO UPDATE SET process_config = EXCLUDED.process_config, updated_at = now()
RETURNING *;
