-- name: RecordQualityCheck :one
-- Keeps failing_since across runs: a rule that was already failing has been failing since then,
-- and a rule that passes forgets, so the next slip is dated from the slip. The prev CTE reads
-- the row as it was before this write — RETURNING would hand back the new one, and then every
-- sweep would look like a fresh regression.
WITH prev AS (
    SELECT status AS previous_status, task_id AS previous_task_id
    FROM quality_checks qc
    WHERE qc.project_id = $1 AND qc.component_id IS NOT DISTINCT FROM sqlc.narg(component_id) AND qc.rule = $2
), up AS (
    INSERT INTO quality_checks (project_id, component_id, rule, source, status, detail, weight, failing_since, checked_at)
    VALUES ($1, sqlc.narg(component_id), $2, $3, $4, $5, $6,
            CASE WHEN $4 = 'fail' THEN now() ELSE NULL END, now())
    ON CONFLICT (project_id, component_id, rule) DO UPDATE
    SET source = EXCLUDED.source, status = EXCLUDED.status, detail = EXCLUDED.detail,
        weight = EXCLUDED.weight, checked_at = now(),
        failing_since = CASE
            WHEN EXCLUDED.status <> 'fail' THEN NULL
            WHEN quality_checks.status = 'fail' THEN quality_checks.failing_since
            ELSE now() END,
        task_id = CASE WHEN EXCLUDED.status <> 'fail' THEN NULL ELSE quality_checks.task_id END
    RETURNING *
)
SELECT up.*, COALESCE(prev.previous_status, '') AS previous_status, prev.previous_task_id
FROM up LEFT JOIN prev ON true;

-- name: SetQualityCheckTask :exec
UPDATE quality_checks SET task_id = $2 WHERE id = $1;

-- name: ListQualityChecks :many
SELECT * FROM quality_checks WHERE project_id = $1 ORDER BY component_id NULLS FIRST, rule;

-- name: RecordQualityScore :one
INSERT INTO quality_scores (project_id, component_id, earned, possible)
VALUES ($1, sqlc.narg(component_id), $2, $3)
RETURNING *;

-- name: LatestQualityScores :many
-- The newest score per component, and the one before it, which is the trend.
SELECT DISTINCT ON (component_id) * FROM quality_scores
WHERE project_id = $1 ORDER BY component_id, checked_at DESC;

-- name: QualityScoreHistory :many
SELECT * FROM quality_scores
WHERE project_id = $1 AND component_id IS NOT DISTINCT FROM sqlc.narg(component_id)
ORDER BY checked_at DESC LIMIT $2;

-- name: ProjectsWithComponents :many
-- Projects worth sweeping: one that has named no parts has nothing to score.
SELECT DISTINCT p.id FROM projects p
JOIN components c ON c.project_id = p.id
WHERE p.archived_at IS NULL;
