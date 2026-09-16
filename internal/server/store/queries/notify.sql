-- name: UpsertDevice :one
-- A device registers on every launch; re-registering revives one Apple had rejected.
INSERT INTO devices (user_id, token, platform, app_version) VALUES ($1, $2, $3, $4)
ON CONFLICT (token) DO UPDATE SET user_id = EXCLUDED.user_id, platform = EXCLUDED.platform,
    app_version = EXCLUDED.app_version, last_seen_at = now(), failed_at = NULL
RETURNING *;

-- name: DeleteDevice :execrows
DELETE FROM devices WHERE token = $1 AND user_id = $2;

-- name: ListDevicesForUser :many
SELECT * FROM devices WHERE user_id = $1 AND failed_at IS NULL ORDER BY last_seen_at DESC;

-- name: ListDevicesForUsers :many
SELECT * FROM devices WHERE user_id = ANY(sqlc.arg(user_ids)::uuid[]) AND failed_at IS NULL;

-- name: MarkDeviceGone :exec
UPDATE devices SET failed_at = now() WHERE token = $1;

-- name: ListProjectMemberIDs :many
SELECT user_id FROM memberships WHERE project_id = $1;

-- name: ListWorkspaceAdminIDs :many
SELECT id FROM users WHERE workspace_role = 'admin';
