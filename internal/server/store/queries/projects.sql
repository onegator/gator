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
