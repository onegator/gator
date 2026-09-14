-- name: ListProjectRepos :many
SELECT * FROM project_repos WHERE project_id = $1 ORDER BY is_primary DESC, name;

-- name: UpsertProjectRepo :one
INSERT INTO project_repos (project_id, name, url, default_branch, is_primary)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id, name) DO UPDATE
SET url = EXCLUDED.url, default_branch = EXCLUDED.default_branch, is_primary = EXCLUDED.is_primary
RETURNING *;

-- name: ClearOtherPrimaryRepos :exec
UPDATE project_repos SET is_primary = false WHERE project_id = $1 AND name <> $2;

-- name: GetPrimaryRepo :one
SELECT * FROM project_repos WHERE project_id = $1 ORDER BY is_primary DESC, name LIMIT 1;
