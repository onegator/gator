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

-- name: InsertInstallation :one
INSERT INTO installations (project_id, registry_id, kind, name, version, checksum, target_id, accepted_permissions, installed, installed_by)
VALUES (sqlc.narg(project_id), sqlc.arg(registry_id), sqlc.arg(kind), sqlc.arg(name), sqlc.arg(version), sqlc.arg(checksum),
        sqlc.narg(target_id), sqlc.arg(accepted_permissions), sqlc.arg(installed), sqlc.arg(installed_by))
RETURNING *;

-- name: GetInstallation :one
SELECT * FROM installations WHERE id = $1;

-- name: GetInstallationForUpdate :one
SELECT * FROM installations WHERE id = $1 FOR UPDATE;

-- name: ListInstallations :many
-- A project's installations (project set) or the workspace's (project NULL).
SELECT * FROM installations
WHERE project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::uuid
ORDER BY kind, name;

-- name: FindInstallation :one
-- What a project has of a package, its own before the workspace's.
SELECT * FROM installations
WHERE kind = sqlc.arg(kind) AND name = sqlc.arg(name)
  AND (project_id = sqlc.arg(project_id) OR project_id IS NULL)
ORDER BY project_id IS NULL
LIMIT 1;

-- name: DeleteInstallation :execrows
DELETE FROM installations WHERE id = $1;

-- name: GetInstallationByTarget :one
SELECT * FROM installations WHERE target_id = $1 LIMIT 1;

-- name: CreateInstalledAgent :one
-- An agent from the marketplace: its tools and skills come with it, and its source says where from.
INSERT INTO agents (project_id, handle, display_name, description, avatar, role_key, instructions, tier, policy, tools, skills,
                    status, source, created_by)
VALUES (sqlc.narg(project_id), sqlc.arg(handle), sqlc.arg(display_name), sqlc.arg(description), sqlc.arg(avatar), sqlc.narg(role_key),
        sqlc.arg(instructions), sqlc.arg(tier), sqlc.arg(policy), sqlc.arg(tools), sqlc.arg(skills), 'active', sqlc.arg(source),
        sqlc.narg(created_by))
RETURNING *;

-- name: ListSkillNamesVisibleIn :many
SELECT name FROM skills WHERE project_id = sqlc.arg(project_id) OR project_id IS NULL;

-- name: GetSkillIn :one
SELECT * FROM skills WHERE project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::uuid AND name = sqlc.arg(name);

-- name: UpsertSkill :one
INSERT INTO skills (project_id, name, version, description, files, source)
VALUES (sqlc.narg(project_id), sqlc.arg(name), sqlc.arg(version), sqlc.arg(description), sqlc.arg(files), sqlc.arg(source))
ON CONFLICT (project_id, name) DO UPDATE
SET version = EXCLUDED.version, description = EXCLUDED.description, files = EXCLUDED.files, source = EXCLUDED.source, updated_at = now()
RETURNING *;

-- name: DeleteSkill :execrows
DELETE FROM skills WHERE id = $1;

-- name: DeleteBundledSkills :exec
-- The skills an agent package brought into a scope, once no agent there carries them any more.
DELETE FROM skills s
WHERE s.project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::uuid AND s.source->>'agent' = sqlc.arg(agent)::text
  AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.deleted_at IS NULL AND a.project_id IS NOT DISTINCT FROM s.project_id AND a.skills ? s.name);

-- name: ListSkillsVisibleIn :many
-- The skills a project's agents may carry: the project's own and the workspace's.
SELECT * FROM skills WHERE project_id = sqlc.arg(project_id) OR project_id IS NULL ORDER BY name, project_id IS NULL;

-- name: SkillsNamed :many
-- The skills of these names an agent of a project carries: the project's own before the
-- workspace's of the same name.
SELECT DISTINCT ON (name) * FROM skills
WHERE name = ANY(sqlc.arg(names)::text[]) AND (project_id = sqlc.arg(project_id) OR project_id IS NULL)
ORDER BY name, project_id IS NULL;

-- name: LockProjectConfig :one
SELECT process_config FROM projects WHERE id = $1 FOR UPDATE;

-- name: UpdateInstallation :one
UPDATE installations
SET version = sqlc.arg(version), checksum = sqlc.arg(checksum), accepted_permissions = sqlc.arg(accepted_permissions),
    installed = sqlc.arg(installed), updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: UpdateInstalledAgent :one
-- An installed agent brought to a new version of its package; its handle, status and who made it stay.
UPDATE agents
SET display_name = sqlc.arg(display_name), description = sqlc.arg(description), avatar = sqlc.arg(avatar), role_key = sqlc.narg(role_key),
    instructions = sqlc.arg(instructions), tier = sqlc.arg(tier), policy = sqlc.arg(policy), tools = sqlc.arg(tools), skills = sqlc.arg(skills),
    source = sqlc.arg(source), updated_at = now()
WHERE id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING *;

-- name: UpdateInstalledMCPServer :one
-- A remote server brought to a new version of its package. A new address, or a new way of
-- signing in, takes its credentials away: they were given for the old one.
UPDATE mcp_servers
SET display_name = sqlc.arg(display_name), url = sqlc.arg(url), auth = sqlc.arg(auth), oauth = sqlc.arg(oauth),
    allowed_tools = sqlc.arg(allowed_tools), source = sqlc.arg(source),
    secrets = CASE WHEN url = sqlc.arg(url) AND auth = sqlc.arg(auth) AND sqlc.arg(keep_credentials)::boolean THEN secrets ELSE '' END,
    token_expires_at = CASE WHEN url = sqlc.arg(url) AND auth = sqlc.arg(auth) AND sqlc.arg(keep_credentials)::boolean THEN token_expires_at END,
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: GetSkill :one
SELECT * FROM skills WHERE id = $1;
