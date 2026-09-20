-- name: UpsertProjectTemplate :one
INSERT INTO project_templates (name, description, payload, created_by)
VALUES ($1, $2, $3, sqlc.narg(created_by))
ON CONFLICT (name) DO UPDATE
SET description = EXCLUDED.description, payload = EXCLUDED.payload, updated_at = now()
RETURNING *;

-- name: ListProjectTemplates :many
SELECT * FROM project_templates ORDER BY name;

-- name: GetProjectTemplate :one
SELECT * FROM project_templates WHERE id = $1;

-- name: GetProjectTemplateByName :one
SELECT * FROM project_templates WHERE name = $1;

-- name: DeleteProjectTemplate :execrows
DELETE FROM project_templates WHERE id = $1;
