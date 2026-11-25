-- name: ListRegistries :many
SELECT * FROM marketplace_registries ORDER BY builtin DESC, name;

-- name: GetRegistry :one
SELECT * FROM marketplace_registries WHERE id = $1;

-- name: CreateRegistry :one
INSERT INTO marketplace_registries (name, url, public_key, created_by)
VALUES (sqlc.arg(name), sqlc.arg(url), sqlc.arg(public_key), sqlc.narg(created_by))
RETURNING *;

-- name: EnsureBuiltinRegistry :one
-- Gator's own registry follows the server's configuration: its address and key are the
-- operator's to change, so the row is brought in line at every start.
INSERT INTO marketplace_registries (name, url, public_key, builtin)
VALUES (sqlc.arg(name), sqlc.arg(url), sqlc.arg(public_key), true)
ON CONFLICT (builtin) WHERE builtin DO UPDATE SET url = EXCLUDED.url, public_key = EXCLUDED.public_key, name = EXCLUDED.name
RETURNING *;

-- name: DeleteRegistry :execrows
DELETE FROM marketplace_registries WHERE id = $1 AND NOT builtin;

-- name: CountRegistryInstallations :one
SELECT count(*)::int FROM installations WHERE registry_id = $1;

-- name: SetRegistryFetched :exec
UPDATE marketplace_registries SET fetched_at = now(), fetch_error = sqlc.arg(fetch_error) WHERE id = sqlc.arg(id);

-- name: SetRegistryFetchError :exec
UPDATE marketplace_registries SET fetch_error = sqlc.arg(fetch_error) WHERE id = sqlc.arg(id);

-- name: GetItemChecksum :one
SELECT checksum FROM marketplace_items
WHERE registry_id = sqlc.arg(registry_id) AND kind = sqlc.arg(kind) AND name = sqlc.arg(name) AND version = sqlc.arg(version);

-- name: UpsertItem :exec
-- A version is published once: the same files again only marks it listed.
INSERT INTO marketplace_items (registry_id, kind, name, version, manifest, checksum, files)
VALUES (sqlc.arg(registry_id), sqlc.arg(kind), sqlc.arg(name), sqlc.arg(version), sqlc.arg(manifest), sqlc.arg(checksum), sqlc.arg(files))
ON CONFLICT (registry_id, kind, name, version) DO UPDATE SET listed = true, fetched_at = now()
WHERE marketplace_items.checksum = EXCLUDED.checksum;

-- name: UnlistItems :exec
-- What the last fetch did not list is no longer offered.
UPDATE marketplace_items SET listed = false
WHERE registry_id = sqlc.arg(registry_id)
  AND NOT ((kind || '/' || name || '@' || version) = ANY(sqlc.arg(keep)::text[]));

-- name: ListItems :many
-- Every version a registry still lists, newest fetched first; the caller keeps the newest
-- version of each package.
SELECT i.registry_id, r.name AS registry_name, i.kind, i.name, i.version, i.manifest, i.checksum, i.fetched_at
FROM marketplace_items i JOIN marketplace_registries r ON r.id = i.registry_id
WHERE i.listed AND (sqlc.narg(kind)::text IS NULL OR i.kind = sqlc.narg(kind))
ORDER BY r.builtin DESC, r.name, i.kind, i.name;

-- name: ListItemVersions :many
SELECT i.registry_id, r.name AS registry_name, i.kind, i.name, i.version, i.manifest, i.checksum, i.listed, i.fetched_at
FROM marketplace_items i JOIN marketplace_registries r ON r.id = i.registry_id
WHERE i.kind = sqlc.arg(kind) AND i.name = sqlc.arg(name)
  AND (sqlc.narg(registry_id)::uuid IS NULL OR i.registry_id = sqlc.narg(registry_id))
ORDER BY r.builtin DESC, r.name;

-- name: GetItem :one
SELECT * FROM marketplace_items
WHERE registry_id = sqlc.arg(registry_id) AND kind = sqlc.arg(kind) AND name = sqlc.arg(name) AND version = sqlc.arg(version);
