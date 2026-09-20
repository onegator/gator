-- name: CreateTask :one
INSERT INTO tasks (project_id, kind, title, description, phase, urgency, owner_kind, owner_id, source_task_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1;

-- name: GetTaskForUpdate :one
SELECT * FROM tasks WHERE id = $1 FOR UPDATE;

-- name: ListOpenTasksByProject :many
SELECT * FROM tasks WHERE project_id = $1 AND closed_at IS NULL ORDER BY urgency, phase_entered_at;

-- name: SetTaskPhase :exec
UPDATE tasks
SET phase = $2, owner_kind = $3, owner_id = $4, blocked_reason = NULL,
    requirements_changed = false, phase_entered_at = now(), updated_at = now(),
    closed_at = CASE WHEN sqlc.arg(close)::boolean THEN now() ELSE NULL END
WHERE id = $1;

-- name: SetTaskOwner :exec
UPDATE tasks SET owner_kind = $2, owner_id = $3, updated_at = now() WHERE id = $1;

-- name: SetTaskBlocked :exec
UPDATE tasks SET blocked_reason = $2, updated_at = now() WHERE id = $1;

-- name: SetTaskRequirementsChanged :exec
UPDATE tasks SET requirements_changed = $2, updated_at = now() WHERE id = $1;

-- name: InsertPhaseTransition :one
INSERT INTO phase_transitions (task_id, from_phase, to_phase, kind, actor_kind, actor_id, reason, evidence)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListPhaseTransitions :many
SELECT * FROM phase_transitions WHERE task_id = $1 ORDER BY id;

-- name: CountRollbacksInto :one
SELECT count(*) FROM phase_transitions WHERE task_id = $1 AND kind = 'rollback' AND to_phase = $2;

-- name: UpsertGate :exec
INSERT INTO gates (task_id, phase) VALUES ($1, $2)
ON CONFLICT (task_id, phase) DO NOTHING;

-- name: GetGate :one
SELECT * FROM gates WHERE task_id = $1 AND phase = $2;

-- name: SetGateChecks :exec
UPDATE gates SET checks = $3, updated_at = now() WHERE task_id = $1 AND phase = $2;

-- name: SetGateHumanApproval :exec
UPDATE gates SET human_approved_by = $3, human_approved_at = now(), updated_at = now()
WHERE task_id = $1 AND phase = $2;

-- name: SetGateBlocked :exec
UPDATE gates SET blocked_reason = $3, blocked_by = $4, updated_at = now() WHERE task_id = $1 AND phase = $2;

-- name: ClearGateApproval :exec
-- A phase entered again must be approved again.
UPDATE gates SET human_approved_by = NULL, human_approved_at = NULL, updated_at = now()
WHERE task_id = $1 AND phase = $2;

-- name: ClearGateBlocked :exec
UPDATE gates SET blocked_reason = NULL, blocked_by = NULL, updated_at = now() WHERE task_id = $1 AND phase = $2;

-- name: CreateArtifact :one
INSERT INTO artifacts (task_id, phase, type, version, content, url)
VALUES ($1, $2, $3,
        COALESCE((SELECT max(version) + 1 FROM artifacts a WHERE a.task_id = $1 AND a.phase = $2 AND a.type = $3), 1),
        $4, $5)
RETURNING *;

-- name: ApproveArtifact :one
UPDATE artifacts SET approved_at = now(), approved_by = $2 WHERE id = $1 RETURNING *;

-- name: ListArtifacts :many
SELECT * FROM artifacts WHERE task_id = $1 ORDER BY phase, type, version;

-- name: ListOpenTasksWithGates :many
SELECT sqlc.embed(t), sqlc.embed(g)
FROM tasks t
JOIN gates g ON g.task_id = t.id AND g.phase = t.phase
-- An archived project stops asking: that is the point of archiving one.
JOIN projects p ON p.id = t.project_id AND p.archived_at IS NULL
WHERE t.closed_at IS NULL
  AND (sqlc.narg(project_id)::uuid IS NULL OR t.project_id = sqlc.narg(project_id)::uuid)
ORDER BY t.urgency, t.phase_entered_at;

-- name: ListOpenUnblockedTasks :many
-- Archived projects queue nothing: the autopilot leaves them alone.
SELECT t.* FROM tasks t JOIN projects p ON p.id = t.project_id AND p.archived_at IS NULL
WHERE t.closed_at IS NULL AND t.blocked_reason IS NULL ORDER BY t.phase_entered_at;

-- name: ApprovePhaseArtifacts :execrows
UPDATE artifacts SET approved_at = now(), approved_by = $3
WHERE task_id = $1 AND phase = $2 AND approved_at IS NULL;

-- name: LastRollbackReason :one
SELECT reason FROM phase_transitions
WHERE task_id = $1 AND kind = 'rollback' AND to_phase = $2
ORDER BY id DESC LIMIT 1;

-- name: ListCurrentPhaseJobs :many
-- The newest job of each open task's current phase entry, for the inbox.
SELECT DISTINCT ON (j.task_id) j.task_id, j.status
FROM jobs j
JOIN tasks t ON t.id = j.task_id
WHERE t.closed_at IS NULL AND j.phase = t.phase AND j.created_at >= t.phase_entered_at
ORDER BY j.task_id, j.created_at DESC;

-- name: ListBudgetBlockedTasks :many
-- Open tasks whose current gate fails the budget check.
SELECT t.id, t.project_id FROM tasks t
JOIN gates g ON g.task_id = t.id AND g.phase = t.phase
WHERE t.closed_at IS NULL AND g.checks @> '[{"name": "budget", "status": "fail"}]'::jsonb;

-- name: MergeTaskExternalRefs :one
UPDATE tasks SET external_refs = external_refs || sqlc.arg(refs)::jsonb, updated_at = now()
WHERE id = sqlc.arg(id) RETURNING *;

-- name: FindTaskByExternalRef :one
SELECT * FROM tasks
WHERE project_id = sqlc.arg(project_id) AND external_refs ->> sqlc.arg(key)::text = sqlc.arg(value)::text
ORDER BY created_at DESC LIMIT 1;

-- name: ListRollbackCounts :many
-- How often each task was sent back into a phase; the inbox shows it on the row.
SELECT task_id, to_phase, count(*)::int AS rollbacks
FROM phase_transitions WHERE kind = 'rollback' GROUP BY task_id, to_phase;

-- name: ListTasksWithStalePendingChecks :many
-- A plugin check sits at "pending" until an event says otherwise, and a webhook that never
-- arrives leaves it there for good — the gate then shows a state that is simply not true.
-- The gate's own updated_at says how long nothing has moved.
SELECT sqlc.embed(t) FROM tasks t
JOIN gates g ON g.task_id = t.id AND g.phase = t.phase
WHERE t.closed_at IS NULL
  AND (NOT sqlc.narg(project_id)::uuid IS NOT NULL OR t.project_id = sqlc.narg(project_id))
  AND g.updated_at < sqlc.arg(before)
  AND EXISTS (
      SELECT 1 FROM jsonb_array_elements(g.checks) c
      WHERE c->>'status' = 'pending' AND c->>'source' LIKE 'plugin:%'
  )
ORDER BY g.updated_at
LIMIT sqlc.arg(max_rows);
