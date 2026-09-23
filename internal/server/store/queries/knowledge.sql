-- name: UpsertKnowledgePack :one
INSERT INTO knowledge_packs (name, version, source, url, checksum, manifest, files)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (name, version) DO UPDATE SET source = EXCLUDED.source, url = EXCLUDED.url,
    checksum = EXCLUDED.checksum, manifest = EXCLUDED.manifest, files = EXCLUDED.files, fetched_at = now()
RETURNING *;

-- name: ListKnowledgePacks :many
SELECT * FROM knowledge_packs ORDER BY name, version;

-- name: GetKnowledgePack :one
SELECT * FROM knowledge_packs WHERE name = $1 AND version = $2;

-- name: ListPacksForProject :many
SELECT p.*, pp.position AS enabled_position FROM project_packs pp
JOIN knowledge_packs p ON p.id = pp.pack_id
WHERE pp.project_id = $1 ORDER BY pp.position, p.name;

-- name: EnablePackForProject :exec
INSERT INTO project_packs (project_id, pack_id, position) VALUES ($1, $2, $3)
ON CONFLICT (project_id, pack_id) DO UPDATE SET position = EXCLUDED.position;

-- name: DisablePackForProject :execrows
DELETE FROM project_packs WHERE project_id = $1 AND pack_id = $2;

-- name: CreateKnowledgeEntry :one
INSERT INTO knowledge_entries (project_id, scope, title, content, position)
VALUES (sqlc.narg(project_id), $1, $2, $3, $4) RETURNING *;

-- name: UpdateKnowledgeEntry :one
UPDATE knowledge_entries SET scope = $2, title = $3, content = $4, position = $5, updated_at = now()
WHERE id = $1 RETURNING *;

-- name: DeleteKnowledgeEntry :execrows
DELETE FROM knowledge_entries WHERE id = $1;

-- name: ListWorkspaceKnowledge :many
SELECT * FROM knowledge_entries WHERE project_id IS NULL ORDER BY position, scope, title;

-- name: ListProjectKnowledge :many
SELECT * FROM knowledge_entries WHERE project_id = $1 ORDER BY position, scope, title;

-- name: GetKnowledgeEntry :one
SELECT * FROM knowledge_entries WHERE id = $1;

-- name: GetNewestPackByName :one
SELECT * FROM knowledge_packs WHERE name = $1 ORDER BY fetched_at DESC LIMIT 1;

-- name: DisablePacksByName :execrows
DELETE FROM project_packs pp USING knowledge_packs p
WHERE pp.pack_id = p.id AND pp.project_id = $1 AND p.name = $2;

-- name: SearchProjectDocuments :many
-- What this project knows, by words: technical notes (its own and the workspace's) and the
-- product context a person approved. For an agent looking for something its job's package did
-- not carry. Scoped to one project, so no job can read its way into another.
(SELECT 'knowledge'::text AS kind, k.scope || ': ' || k.title AS title, k.content FROM knowledge_entries k
 WHERE (k.project_id = $1 OR k.project_id IS NULL)
   AND (k.content ILIKE '%' || sqlc.arg(query)::text || '%' OR k.title ILIKE '%' || sqlc.arg(query)::text || '%')
 ORDER BY k.position LIMIT sqlc.arg(max_rows))
UNION ALL
(SELECT 'product'::text AS kind, p.kind || ': ' || p.title AS title, p.content FROM product_context p
 WHERE p.project_id = $1 AND p.status = 'approved' AND p.archived_at IS NULL
   AND (p.content ILIKE '%' || sqlc.arg(query)::text || '%' OR p.title ILIKE '%' || sqlc.arg(query)::text || '%')
 LIMIT sqlc.arg(max_rows));
