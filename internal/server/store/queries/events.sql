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

-- name: AggregateProject :one
-- The project an event's aggregate belongs to, for deciding who may see the event. NULL: the
-- aggregate belongs to no project (a runner) or no longer exists.
SELECT COALESCE(
    (SELECT t.project_id FROM tasks t WHERE sqlc.arg(aggregate)::text = 'task' AND t.id = sqlc.arg(id)::uuid),
    (SELECT j.project_id FROM jobs j WHERE sqlc.arg(aggregate)::text = 'job' AND j.id = sqlc.arg(id)::uuid),
    (SELECT r.project_id FROM releases r WHERE sqlc.arg(aggregate)::text = 'release' AND r.id = sqlc.arg(id)::uuid),
    (SELECT i.project_id FROM incidents i WHERE sqlc.arg(aggregate)::text = 'incident' AND i.id = sqlc.arg(id)::uuid),
    (SELECT p.id FROM projects p WHERE sqlc.arg(aggregate)::text = 'project' AND p.id = sqlc.arg(id)::uuid)
)::uuid AS project_id;
