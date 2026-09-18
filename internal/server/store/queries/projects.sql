-- name: CreateProject :one
INSERT INTO projects (slug, name, tags, process_config)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetProjectBySlug :one
SELECT * FROM projects WHERE slug = $1;

-- name: ListProjects :many
SELECT * FROM projects ORDER BY name;

-- name: GetProject :one
SELECT * FROM projects WHERE id = $1;

-- name: UpdateProjectConfig :one
UPDATE projects SET process_config = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: ListProjectMembersWithUsers :many
SELECT m.role, u.id, u.name, u.email FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.project_id = $1 ORDER BY u.name;

-- name: ListUsers :many
SELECT id, name, email, workspace_role, created_at FROM users ORDER BY name;
