-- name: OpenCondensationTask :one
-- Whether this project already has a curation task open for this kind, so a slow agent does not
-- collect a queue of identical tasks.
SELECT count(*)::bigint FROM tasks
WHERE project_id = $1 AND kind = 'curation' AND closed_at IS NULL AND title = $2;
