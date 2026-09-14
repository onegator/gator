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
WHERE t.closed_at IS NULL
  AND (sqlc.narg(project_id)::uuid IS NULL OR t.project_id = sqlc.narg(project_id)::uuid)
ORDER BY t.urgency, t.phase_entered_at;

-- name: ListOpenUnblockedTasks :many
SELECT * FROM tasks WHERE closed_at IS NULL AND blocked_reason IS NULL ORDER BY phase_entered_at;

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
