-- name: CreateProductEntry :one
-- Entries are versioned per (project, kind, title); a new one never overwrites the old.
INSERT INTO product_context (project_id, kind, title, content, version, status, source_task_id, created_by_kind, created_by, approved_by, approved_at)
VALUES (
    sqlc.arg(project_id), sqlc.arg(kind), sqlc.arg(title), sqlc.arg(content),
    COALESCE((SELECT max(version) + 1 FROM product_context p
              WHERE p.project_id = sqlc.arg(project_id) AND p.kind = sqlc.arg(kind) AND p.title = sqlc.arg(title)), 1),
    sqlc.arg(status), sqlc.narg(source_task_id), sqlc.arg(created_by_kind), sqlc.narg(created_by),
    sqlc.narg(approved_by), sqlc.narg(approved_at)
)
RETURNING *;

-- name: ListProductContext :many
SELECT * FROM product_context WHERE project_id = $1 ORDER BY kind, title, version DESC;

-- name: ListApprovedProductContext :many
-- The newest approved version of each entry; this is what a job's prompt may carry.
SELECT DISTINCT ON (kind, title) * FROM product_context
WHERE project_id = $1 AND status = 'approved'
ORDER BY kind, title, version DESC;

-- name: GetProductEntry :one
SELECT * FROM product_context WHERE id = $1;

-- name: SetProductEntryStatus :one
UPDATE product_context SET status = sqlc.arg(status), approved_by = sqlc.narg(approved_by),
    approved_at = CASE WHEN sqlc.arg(status)::text = 'approved' THEN now() ELSE approved_at END
WHERE id = sqlc.arg(id) RETURNING *;

-- name: CountProposalsForTask :one
-- Keeps a repeated event from proposing the same thing twice.
SELECT count(*) FROM product_context WHERE project_id = $1 AND kind = $2 AND source_task_id = $3;
