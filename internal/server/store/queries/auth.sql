-- name: UpsertUserByOIDC :one
INSERT INTO users (email, name, oidc_subject)
VALUES ($1, $2, $3)
ON CONFLICT (oidc_subject) DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: SetWorkspaceRole :exec
UPDATE users SET workspace_role = $2 WHERE id = $1;

-- name: InsertToken :one
INSERT INTO api_tokens (kind, scope, hash, user_id, project_id, name, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetTokenByHash :one
SELECT * FROM api_tokens
WHERE hash = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now());

-- name: TouchToken :exec
UPDATE api_tokens SET last_used_at = now() WHERE id = $1;

-- name: RevokeToken :exec
UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: ListTokensForUser :many
SELECT * FROM api_tokens WHERE user_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC;

-- name: ListTokensByKind :many
SELECT * FROM api_tokens WHERE kind = $1 AND revoked_at IS NULL ORDER BY created_at DESC;

-- name: GetMembership :one
SELECT * FROM memberships WHERE project_id = $1 AND user_id = $2;

-- name: UpsertMembership :exec
INSERT INTO memberships (project_id, user_id, role) VALUES ($1, $2, $3)
ON CONFLICT (project_id, user_id) DO UPDATE SET role = EXCLUDED.role;

-- name: ListMembershipsForUser :many
SELECT * FROM memberships WHERE user_id = $1;

-- name: GetTaskProject :one
SELECT project_id FROM tasks WHERE id = $1;

-- name: InsertAudit :exec
INSERT INTO audit_log (actor_kind, actor_id, action, target, payload)
VALUES ($1, $2, $3, $4, $5);
