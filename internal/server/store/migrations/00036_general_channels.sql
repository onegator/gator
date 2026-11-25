-- +goose Up
-- Every project has #general from the start; projects made before conversations get theirs here,
-- with everyone in the project already in it.
INSERT INTO conversations (project_id, kind, name, topic, visibility, created_by_kind)
SELECT p.id, 'channel', 'general', '', 'project', 'system' FROM projects p
ON CONFLICT (project_id, name) WHERE kind = 'channel' DO NOTHING;

INSERT INTO conversation_members (conversation_id, member_kind, member_id)
SELECT c.id, 'user', m.user_id
FROM conversations c JOIN memberships m ON m.project_id = c.project_id
WHERE c.kind = 'channel' AND c.name = 'general'
ON CONFLICT DO NOTHING;

-- +goose Down
-- Only the channels nobody has written in go; a #general with messages stays.
DELETE FROM conversations WHERE kind = 'channel' AND name = 'general' AND last_seq = 0 AND created_by_kind = 'system';
