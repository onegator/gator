-- name: InsertEvent :one
INSERT INTO events (type, aggregate, aggregate_id, payload)
VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: ListUnpublishedEvents :many
-- Each event carries the project it happened in, so a board can listen to its project alone
-- rather than to the whole workspace.
SELECT e.*, COALESCE(t.project_id, CASE WHEN e.aggregate = 'project' THEN e.aggregate_id END)::uuid AS project_id
FROM events e
LEFT JOIN tasks t ON e.aggregate = 'task' AND t.id = e.aggregate_id
WHERE e.published_at IS NULL ORDER BY e.id LIMIT $1;

-- name: MarkEventPublished :exec
UPDATE events SET published_at = now() WHERE id = $1;
