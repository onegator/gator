-- name: RecordRelease :one
-- The same version deployed twice to the same place is one release, redeployed.
INSERT INTO releases (project_id, task_id, version, commit_sha, url, environment, source_plugin_id, observation_until)
VALUES ($1, sqlc.narg(task_id), $2, $3, $4, $5, sqlc.narg(source_plugin_id), sqlc.narg(observation_until))
ON CONFLICT (project_id, version, environment) DO UPDATE
SET commit_sha = EXCLUDED.commit_sha, url = EXCLUDED.url, task_id = COALESCE(EXCLUDED.task_id, releases.task_id),
    deployed_at = now(), observation_until = EXCLUDED.observation_until, settled_at = NULL
RETURNING *;

-- name: GetRelease :one
SELECT * FROM releases WHERE id = $1;

-- name: NewestRelease :one
SELECT * FROM releases WHERE project_id = $1 AND environment = $2 ORDER BY deployed_at DESC LIMIT 1;

-- name: ListSettledReleases :many
-- Releases whose observation window has run out without anyone marking them settled. Each is
-- a task waiting to be called finished.
SELECT * FROM releases
WHERE settled_at IS NULL AND observation_until IS NOT NULL AND observation_until < sqlc.arg(now)
ORDER BY observation_until
LIMIT sqlc.arg(max_rows);

-- name: SettleRelease :exec
UPDATE releases SET settled_at = now() WHERE id = $1;

-- name: CountOpenIncidentsForRelease :one
SELECT count(*) FROM incidents WHERE release_id = $1 AND closed_at IS NULL;

-- name: UpsertIncident :one
-- One fault, one incident, however many times the monitoring tool says it.
INSERT INTO incidents (project_id, source_plugin_id, external_id, fingerprint, title, severity, url, release_id)
VALUES ($1, sqlc.narg(source_plugin_id), $2, $3, $4, $5, $6, sqlc.narg(release_id))
ON CONFLICT (project_id, fingerprint) DO UPDATE
SET count = incidents.count + 1, last_seen_at = now(), severity = EXCLUDED.severity,
    title = EXCLUDED.title, url = EXCLUDED.url,
    release_id = COALESCE(EXCLUDED.release_id, incidents.release_id),
    -- A fault that comes back after being closed is open again.
    closed_at = NULL
RETURNING *, (xmax = 0) AS is_new;

-- name: SetIncidentTask :exec
UPDATE incidents SET task_id = $2 WHERE id = $1;

-- name: CloseIncident :one
UPDATE incidents SET closed_at = now() WHERE project_id = $1 AND fingerprint = $2 AND closed_at IS NULL RETURNING *;

-- name: ListIncidents :many
SELECT * FROM incidents WHERE project_id = $1 ORDER BY closed_at NULLS FIRST, last_seen_at DESC LIMIT $2;

-- name: IncidentForTask :one
SELECT * FROM incidents WHERE task_id = $1;

-- name: CloseIncidentByTask :one
UPDATE incidents SET closed_at = now() WHERE task_id = $1 AND closed_at IS NULL RETURNING *;

-- name: ListReleases :many
SELECT * FROM releases WHERE project_id = $1 ORDER BY deployed_at DESC LIMIT $2;
