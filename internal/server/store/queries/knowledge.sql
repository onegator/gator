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

-- name: GetNewestPackByName :one
SELECT * FROM knowledge_packs WHERE name = $1 ORDER BY fetched_at DESC LIMIT 1;

-- name: DisablePacksByName :execrows
DELETE FROM project_packs pp USING knowledge_packs p
WHERE pp.pack_id = p.id AND pp.project_id = $1 AND p.name = $2;
