-- name: UpsertComponent :one
INSERT INTO components (project_id, key, name, kind, repo, path, owner_id, notes)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg(owner_id), $7)
ON CONFLICT (project_id, key) DO UPDATE
SET name = EXCLUDED.name, kind = EXCLUDED.kind, repo = EXCLUDED.repo, path = EXCLUDED.path,
    owner_id = EXCLUDED.owner_id, notes = EXCLUDED.notes, updated_at = now()
RETURNING *;

-- name: ListComponents :many
SELECT * FROM components WHERE project_id = $1 ORDER BY kind, key;

-- name: GetComponent :one
SELECT * FROM components WHERE id = $1;

-- name: GetComponentByKey :one
SELECT * FROM components WHERE project_id = $1 AND key = $2;

-- name: DeleteComponent :execrows
DELETE FROM components WHERE id = $1 AND project_id = $2;

-- name: SetComponentDep :exec
-- Both sides are checked against the project, so a dependency can never cross into another.
INSERT INTO component_deps (component_id, depends_on_id)
SELECT c.id, d.id FROM components c, components d
WHERE c.id = sqlc.arg(component_id) AND d.id = sqlc.arg(depends_on_id) AND c.project_id = d.project_id
ON CONFLICT DO NOTHING;

-- name: RemoveComponentDep :execrows
DELETE FROM component_deps WHERE component_id = $1 AND depends_on_id = $2;

-- name: ListComponentDeps :many
SELECT sqlc.embed(c), d.component_id AS of_id FROM component_deps d
JOIN components c ON c.id = d.depends_on_id
WHERE d.component_id = ANY(sqlc.arg(component_ids)::uuid[])
ORDER BY c.key;

-- name: ListComponentDependents :many
-- Who breaks if this component changes. A job touching a library should know.
SELECT sqlc.embed(c), d.depends_on_id AS of_id FROM component_deps d
JOIN components c ON c.id = d.component_id
WHERE d.depends_on_id = ANY(sqlc.arg(component_ids)::uuid[])
ORDER BY c.key;

-- name: LinkComponentDecision :exec
INSERT INTO component_decisions (component_id, entry_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: UnlinkComponentDecision :execrows
DELETE FROM component_decisions WHERE component_id = $1 AND entry_id = $2;

-- name: ListComponentDecisions :many
-- Only approved entries: a proposal nobody accepted is not yet how this component works.
SELECT sqlc.embed(p), l.component_id AS of_id FROM component_decisions l
JOIN product_context p ON p.id = l.entry_id
WHERE l.component_id = ANY(sqlc.arg(component_ids)::uuid[]) AND p.status = 'approved'
ORDER BY p.kind, p.title;

-- name: SetTaskComponent :one
UPDATE tasks SET component_id = sqlc.narg(component_id), updated_at = now()
WHERE id = sqlc.arg(id) RETURNING *;

-- name: SetJobContextSize :exec
UPDATE jobs SET context_bytes = $2, context_docs = $3, updated_at = now() WHERE id = $1;

-- name: RecordComponentHint :one
-- One job's evidence about one directory. jobs counts distinct jobs, which is what tells a real
-- part of the product from a file somebody passed through once.
INSERT INTO component_hints (project_id, path, jobs, files, last_job_id, last_seen_at)
VALUES ($1, $2, 1, sqlc.arg(files), sqlc.narg(last_job_id), now())
ON CONFLICT (project_id, path) DO UPDATE
SET jobs = component_hints.jobs + CASE WHEN component_hints.last_job_id IS DISTINCT FROM EXCLUDED.last_job_id THEN 1 ELSE 0 END,
    files = component_hints.files + EXCLUDED.files,
    last_job_id = EXCLUDED.last_job_id,
    last_seen_at = now()
RETURNING *;

-- name: MarkHintProposed :exec
UPDATE component_hints SET proposed_at = now() WHERE id = $1;

-- name: ListApprovedComponents :many
SELECT * FROM components WHERE project_id = $1 AND status = 'approved' ORDER BY kind, key;

-- name: SetComponentStatus :one
UPDATE components SET status = $2, proposed_reason = CASE WHEN $2 = 'approved' THEN '' ELSE proposed_reason END,
    updated_at = now()
WHERE project_id = $1 AND key = sqlc.arg(key) RETURNING *;

-- name: ProposeComponent :one
INSERT INTO components (project_id, key, name, kind, repo, path, notes, status, proposed_reason)
VALUES ($1, $2, $3, $4, $5, $6, '', 'proposed', sqlc.arg(proposed_reason))
ON CONFLICT (project_id, key) DO NOTHING
RETURNING *;
