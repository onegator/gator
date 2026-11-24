-- +goose Up
-- Every task has its thread. Open tasks made before conversations get one here, quietly: no card
-- is posted into #general for work that is already under way, so the thread has no root message.
INSERT INTO conversations (project_id, kind, task_id, visibility, created_by_kind)
SELECT t.project_id, 'task', t.id, 'project', 'system' FROM tasks t
WHERE t.closed_at IS NULL
ON CONFLICT (task_id) DO NOTHING;

-- +goose Down
DELETE FROM conversations
WHERE kind = 'task' AND parent_message_id IS NULL AND last_seq = 0 AND created_by_kind = 'system';
