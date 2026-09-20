-- name: AddCampaignTarget :one
INSERT INTO campaign_targets (campaign_id, target_key, project_id, task_id)
VALUES ($1, $2, $3, sqlc.narg(task_id))
ON CONFLICT (campaign_id, target_key) DO UPDATE SET task_id = COALESCE(EXCLUDED.task_id, campaign_targets.task_id)
RETURNING *;

-- name: ListCampaignTargets :many
SELECT sqlc.embed(t), tk.phase, tk.closed_at, tk.blocked_reason, tk.title
FROM campaign_targets t
LEFT JOIN tasks tk ON tk.id = t.task_id
WHERE t.campaign_id = $1
ORDER BY t.target_key;

-- name: SkipCampaignTarget :one
UPDATE campaign_targets SET skipped_at = now(), skip_reason = $3
WHERE campaign_id = $1 AND target_key = $2 AND skipped_at IS NULL
RETURNING *;

-- name: CampaignForChild :one
SELECT campaign_id FROM campaign_targets WHERE task_id = $1;

-- name: ListCampaignChildIDs :many
-- Children of open campaigns. Their approvals belong to the campaign's one decision, so the
-- inbox does not ask about each of them separately.
SELECT t.task_id FROM campaign_targets t
JOIN tasks c ON c.id = t.campaign_id
WHERE c.closed_at IS NULL AND t.task_id IS NOT NULL;
