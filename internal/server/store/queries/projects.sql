-- name: CreateProject :one
INSERT INTO projects (slug, name, tags, process_config)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetProjectBySlug :one
SELECT * FROM projects WHERE slug = $1;

-- name: ListProjects :many
-- Live projects. An archived one is still readable by id, so its metrics and history survive.
SELECT * FROM projects WHERE archived_at IS NULL ORDER BY name;

-- name: ListAllProjects :many
SELECT * FROM projects ORDER BY archived_at NULLS FIRST, name;

-- name: SetProjectArchived :one
UPDATE projects SET archived_at = CASE WHEN sqlc.arg(archived)::boolean THEN now() ELSE NULL END,
    updated_at = now()
WHERE id = sqlc.arg(id) RETURNING *;

-- name: CountTasksInProject :one
SELECT count(*) FROM tasks WHERE project_id = $1;

-- name: DeleteProject :execrows
-- Only an empty project: everything else is archived instead, so no history is ever lost to
-- a click.
DELETE FROM projects p WHERE p.id = sqlc.arg(id)
  AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.project_id = sqlc.arg(id));

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
