-- name: UpsertPlugin :one
INSERT INTO plugins (name, command, version, manifest) VALUES ($1, $2, $3, $4)
ON CONFLICT (name) DO UPDATE SET command = EXCLUDED.command, version = EXCLUDED.version,
    manifest = EXCLUDED.manifest, updated_at = now()
RETURNING *;

-- name: ListPlugins :many
SELECT * FROM plugins ORDER BY name;

-- name: GetPluginByName :one
SELECT * FROM plugins WHERE name = $1;

-- name: UpsertProjectPlugin :one
INSERT INTO project_plugins (project_id, plugin_id, config, secrets, enabled) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id, plugin_id) DO UPDATE SET config = EXCLUDED.config, secrets = EXCLUDED.secrets,
    enabled = EXCLUDED.enabled, disabled_reason = NULL, updated_at = now()
RETURNING *;

-- name: GetProjectPlugin :one
SELECT pp.id, pp.project_id, pp.config, pp.secrets, pp.enabled, pp.disabled_reason,
       p.name AS plugin_name, p.command, p.manifest, p.enabled AS plugin_enabled, pr.slug AS project_slug
FROM project_plugins pp JOIN plugins p ON p.id = pp.plugin_id JOIN projects pr ON pr.id = pp.project_id
WHERE pp.project_id = $1 AND p.name = $2;

-- name: GetProjectPluginBySlug :one
SELECT pp.id, pp.project_id, pp.config, pp.secrets, pp.enabled, pp.disabled_reason,
       p.name AS plugin_name, p.command, p.manifest, p.enabled AS plugin_enabled, pr.slug AS project_slug
FROM project_plugins pp JOIN plugins p ON p.id = pp.plugin_id JOIN projects pr ON pr.id = pp.project_id
WHERE pr.slug = $1 AND p.name = $2;

-- name: ListProjectPlugins :many
SELECT pp.id, pp.project_id, pp.config, pp.secrets, pp.enabled, pp.disabled_reason,
       p.name AS plugin_name, p.command, p.manifest, p.enabled AS plugin_enabled, pr.slug AS project_slug
FROM project_plugins pp JOIN plugins p ON p.id = pp.plugin_id JOIN projects pr ON pr.id = pp.project_id
WHERE pp.project_id = $1 ORDER BY p.name;

-- name: ListActiveProjectPlugins :many
SELECT pp.id, pp.project_id, pp.config, pp.secrets, pp.enabled, pp.disabled_reason,
       p.name AS plugin_name, p.command, p.manifest, p.enabled AS plugin_enabled, pr.slug AS project_slug
FROM project_plugins pp JOIN plugins p ON p.id = pp.plugin_id JOIN projects pr ON pr.id = pp.project_id
WHERE pp.enabled AND p.enabled;

-- name: DisableProjectPlugin :exec
UPDATE project_plugins SET enabled = false, disabled_reason = $2, updated_at = now() WHERE id = $1;

-- name: ProjectCapabilities :many
SELECT DISTINCT c::text AS capability
FROM project_plugins pp JOIN plugins p ON p.id = pp.plugin_id,
     jsonb_array_elements_text(p.manifest->'capabilities') AS c
WHERE pp.project_id = $1 AND pp.enabled AND p.enabled
ORDER BY 1;

-- name: InsertPluginCall :exec
INSERT INTO plugin_calls (project_plugin_id, direction, method, payload, result, error, duration_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListPluginCalls :many
SELECT * FROM plugin_calls WHERE project_plugin_id = $1 ORDER BY id DESC LIMIT $2;

-- name: ClaimWebhookDelivery :execrows
INSERT INTO webhook_deliveries (project_plugin_id, delivery_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: ReleaseWebhookDelivery :exec
DELETE FROM webhook_deliveries WHERE project_plugin_id = $1 AND delivery_id = $2;

-- name: PluginKVGet :one
SELECT value FROM plugin_kv WHERE project_plugin_id = $1 AND key = $2;

-- name: PluginKVPut :exec
INSERT INTO plugin_kv (project_plugin_id, key, value) VALUES ($1, $2, $3)
ON CONFLICT (project_plugin_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: InitEventCursor :exec
-- A consumer's first boot starts at the newest event: it does not replay history.
INSERT INTO event_cursors (name, last_event_id)
SELECT sqlc.arg(name), COALESCE(max(id), 0) FROM events
ON CONFLICT (name) DO NOTHING;

-- name: GetEventCursor :one
SELECT last_event_id FROM event_cursors WHERE name = $1;

-- name: SetEventCursor :exec
UPDATE event_cursors SET last_event_id = sqlc.arg(event_id), updated_at = now()
WHERE name = sqlc.arg(name) AND last_event_id < sqlc.arg(event_id);

-- name: EventsAfter :many
SELECT id, type, aggregate, aggregate_id, payload FROM events
WHERE id > sqlc.arg(after) AND type = ANY(sqlc.arg(types)::text[])
ORDER BY id LIMIT sqlc.arg(max_rows);
