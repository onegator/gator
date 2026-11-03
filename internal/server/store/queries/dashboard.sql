-- name: ListDashboardSources :many
-- The plugins whose numbers the dashboard counts: switched on, in a live project, saying they
-- track issues or watch for errors.
SELECT pp.id, pp.project_id, p.name AS plugin_name,
       COALESCE((p.manifest -> 'capabilities') ? 'tracker', false)::bool AS tracker,
       COALESCE((p.manifest -> 'capabilities') ? 'monitoring', false)::bool AS monitoring
FROM project_plugins pp
JOIN plugins p ON p.id = pp.plugin_id
JOIN projects pr ON pr.id = pp.project_id
WHERE pp.enabled AND p.enabled AND pr.archived_at IS NULL
  AND ((p.manifest -> 'capabilities') ? 'tracker' OR (p.manifest -> 'capabilities') ? 'monitoring')
  AND (sqlc.narg(project_id)::uuid IS NULL OR pp.project_id = sqlc.narg(project_id)::uuid)
ORDER BY pr.name, p.name;

-- name: CountOpenTasksByOrigin :many
-- Open tasks per project and the plugin that raised them.
SELECT project_id, origin_source, count(*)::int AS open
FROM tasks
WHERE closed_at IS NULL AND origin_source <> ''
  AND (sqlc.narg(project_id)::uuid IS NULL OR project_id = sqlc.narg(project_id)::uuid)
GROUP BY project_id, origin_source;

-- name: CountOpenIncidentsByPlugin :many
-- Open incidents per project plugin that reported them.
SELECT source_plugin_id, count(*)::int AS open
FROM incidents
WHERE closed_at IS NULL AND source_plugin_id IS NOT NULL
  AND (sqlc.narg(project_id)::uuid IS NULL OR project_id = sqlc.narg(project_id)::uuid)
GROUP BY source_plugin_id;
