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
-- The newest approved version of each entry; this is what a job's prompt may carry. An
-- archived entry is still readable, but it has been folded into a condensed one and no longer
-- spends anybody's context budget.
SELECT DISTINCT ON (kind, title) * FROM product_context
WHERE project_id = $1 AND status = 'approved' AND archived_at IS NULL
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

-- name: LiveProductEntriesOfKind :many
-- What a condensation would fold together: approved, not archived, not itself a condensate.
SELECT DISTINCT ON (title) * FROM product_context
WHERE project_id = $1 AND kind = $2 AND status = 'approved' AND archived_at IS NULL
  AND cardinality(condensed_from) = 0
ORDER BY title, version DESC;

-- name: ProductKindsWorthCondensing :many
-- Kinds whose live entries have grown past what a prompt should carry, with how much they hold.
SELECT project_id, kind, count(*)::bigint AS entries, coalesce(sum(length(content)), 0)::bigint AS bytes
FROM (SELECT DISTINCT ON (project_id, kind, title) project_id, kind, title, content
      FROM product_context
      WHERE status = 'approved' AND archived_at IS NULL AND cardinality(condensed_from) = 0
      ORDER BY project_id, kind, title, version DESC) live
GROUP BY project_id, kind
HAVING count(*) >= sqlc.arg(min_entries)::bigint OR coalesce(sum(length(content)), 0) >= sqlc.arg(min_bytes)::bigint;

-- name: CreateCondensedEntry :one
INSERT INTO product_context (project_id, kind, title, content, version, status, source_task_id, created_by_kind, condensed_from)
VALUES ($1, $2, $3, $4,
    COALESCE((SELECT max(version) + 1 FROM product_context p
              WHERE p.project_id = $1 AND p.kind = $2 AND p.title = $3), 1),
    'proposed', sqlc.narg(source_task_id), 'system', sqlc.arg(condensed_from))
RETURNING *;

-- name: ArchiveProductEntries :execrows
UPDATE product_context SET archived_at = now()
WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND archived_at IS NULL;

-- name: OpenCondensationTask :one
-- Whether this project already has a curation task open for this kind, so a slow agent does not
-- collect a queue of identical tasks.
SELECT count(*)::bigint FROM tasks
WHERE project_id = $1 AND kind = 'curation' AND closed_at IS NULL AND title = $2;
