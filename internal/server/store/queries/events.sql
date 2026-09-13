-- name: InsertEvent :one
INSERT INTO events (type, aggregate, aggregate_id, payload)
VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: ListUnpublishedEvents :many
SELECT * FROM events WHERE published_at IS NULL ORDER BY id LIMIT $1;

-- name: MarkEventPublished :exec
UPDATE events SET published_at = now() WHERE id = $1;
