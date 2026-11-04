-- name: ListProjectAutomations :many
SELECT * FROM project_automations WHERE project_id = $1 ORDER BY key;

-- name: GetProjectAutomation :one
SELECT * FROM project_automations WHERE project_id = $1 AND key = $2;

-- name: EnableProjectAutomation :one
-- Switching on, or changing the settings of one already on. Whoever switches it on is the person
-- the tasks it creates name as having allowed agents; changing only the settings keeps them.
INSERT INTO project_automations (project_id, key, enabled, settings, enabled_by, enabled_at, last_error, updated_at)
VALUES ($1, $2, true, $3, $4, now(), '', now())
ON CONFLICT (project_id, key) DO UPDATE
SET settings = EXCLUDED.settings,
    enabled_by = CASE WHEN project_automations.enabled THEN project_automations.enabled_by ELSE EXCLUDED.enabled_by END,
    enabled_at = CASE WHEN project_automations.enabled THEN project_automations.enabled_at ELSE now() END,
    last_error = CASE WHEN project_automations.enabled THEN project_automations.last_error ELSE '' END,
    enabled = true,
    updated_at = now()
RETURNING *;

-- name: DisableProjectAutomation :one
-- Switching off keeps the settings, so switching on again starts from where it was.
INSERT INTO project_automations (project_id, key, enabled, settings, updated_at)
VALUES ($1, $2, false, $3, now())
ON CONFLICT (project_id, key) DO UPDATE
SET enabled = false, settings = EXCLUDED.settings, updated_at = now()
RETURNING *;

-- name: ListEnabledAutomations :many
-- Every automation switched on, in projects that still run things.
SELECT pa.* FROM project_automations pa JOIN projects p ON p.id = pa.project_id
WHERE pa.enabled AND p.archived_at IS NULL AND pa.key = ANY(sqlc.arg(keys)::text[])
ORDER BY pa.project_id, pa.key;

-- name: RecordAutomationRun :exec
UPDATE project_automations SET last_run_at = now(), last_error = '' WHERE project_id = $1 AND key = $2;

-- name: SetAutomationError :exec
UPDATE project_automations SET last_error = $3 WHERE project_id = $1 AND key = $2;

-- name: ClaimAutomationRun :one
-- The run for one subject. Nothing comes back when the subject already had its run: a webhook
-- delivered twice, or a week already done before the server restarted.
INSERT INTO automation_runs (project_id, automation, subject)
VALUES ($1, $2, $3)
ON CONFLICT (project_id, automation, subject) DO NOTHING
RETURNING *;

-- name: SetAutomationRunTask :exec
UPDATE automation_runs SET task_id = $2 WHERE id = $1;

-- name: GetAutomationRun :one
SELECT * FROM automation_runs WHERE project_id = $1 AND automation = $2 AND subject = $3;

-- name: CountOpenAutomationTasksByRef :one
-- Open tasks of one automation about the same thing, by the reference the plugin keeps on them:
-- a second review of a pull request waits until the first is finished.
SELECT count(*)::int FROM tasks
WHERE project_id = sqlc.arg(project_id) AND automation = sqlc.arg(automation)::text AND closed_at IS NULL
  AND external_refs ->> sqlc.arg(key)::text = sqlc.arg(value)::text;

-- name: SetTaskAutomation :one
-- Which automation's work a task is. The consent its enabling gave is recorded separately, by
-- the same queries a person's own answer uses.
UPDATE tasks SET automation = sqlc.arg(automation)::text WHERE id = sqlc.arg(id) RETURNING *;

-- name: LatestAutomationTasks :many
-- The newest task each of a project's automations created, for "open the latest".
SELECT DISTINCT ON (automation) automation, task_id FROM automation_runs
WHERE project_id = $1 AND task_id IS NOT NULL
ORDER BY automation, created_at DESC;

-- name: ListOpenTasksOfAutomation :many
SELECT * FROM tasks WHERE project_id = $1 AND automation = sqlc.arg(automation)::text AND closed_at IS NULL;

-- name: ListTasksClosedBetween :many
SELECT * FROM tasks
WHERE project_id = sqlc.arg(project_id) AND closed_at >= sqlc.arg(since) AND closed_at < sqlc.arg(until)
ORDER BY closed_at DESC;

-- name: CountTasksCreatedBetween :one
SELECT count(*)::int FROM tasks
WHERE project_id = sqlc.arg(project_id) AND created_at >= sqlc.arg(since) AND created_at < sqlc.arg(until);

-- name: ProjectUsageBetween :one
-- What agents did in a project over a window: their time, what it cost and how many jobs.
SELECT COALESCE(sum(duration_ms), 0)::bigint AS agent_ms,
       COALESCE(sum(cost_usd), 0)::double precision AS cost_usd,
       count(DISTINCT job_id)::int AS jobs
FROM usage_records
WHERE project_id = sqlc.arg(project_id) AND created_at >= sqlc.arg(since) AND created_at < sqlc.arg(until);

-- name: CountProjectRepos :one
SELECT count(*)::int FROM project_repos WHERE project_id = $1;
