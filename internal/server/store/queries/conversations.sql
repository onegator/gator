-- name: CreateConversation :one
INSERT INTO conversations (project_id, kind, name, topic, visibility, parent_message_id, task_id, component_key, dm_key,
                           created_by_kind, created_by)
VALUES (sqlc.narg(project_id), sqlc.arg(kind), sqlc.narg(name), sqlc.arg(topic), sqlc.arg(visibility),
        sqlc.narg(parent_message_id), sqlc.narg(task_id), sqlc.narg(component_key), sqlc.narg(dm_key),
        sqlc.arg(created_by_kind), sqlc.narg(created_by))
RETURNING *;

-- name: CreateChannelIfMissing :one
-- A channel the system keeps (#general): nothing comes back when it is already there.
INSERT INTO conversations (project_id, kind, name, topic, visibility, created_by_kind)
VALUES (sqlc.arg(project_id), 'channel', sqlc.arg(name)::text, sqlc.arg(topic), 'project', 'system')
ON CONFLICT (project_id, name) WHERE kind = 'channel' DO NOTHING
RETURNING *;

-- name: CreateDMIfMissing :one
-- Nothing comes back when these people already have a DM in this scope.
INSERT INTO conversations (project_id, kind, visibility, dm_key, created_by_kind, created_by)
VALUES (sqlc.narg(project_id), 'dm', 'private', sqlc.arg(dm_key)::text, 'user', sqlc.arg(created_by))
ON CONFLICT (project_id, dm_key) WHERE kind = 'dm' DO NOTHING
RETURNING *;

-- name: GetConversation :one
SELECT * FROM conversations WHERE id = $1;

-- name: GetChannelByName :one
SELECT * FROM conversations WHERE project_id = sqlc.arg(project_id) AND kind = 'channel' AND name = sqlc.arg(name)::text;

-- name: GetDM :one
SELECT * FROM conversations
WHERE kind = 'dm' AND project_id IS NOT DISTINCT FROM sqlc.narg(project_id) AND dm_key = sqlc.arg(dm_key)::text;

-- name: GetTaskConversation :one
SELECT * FROM conversations WHERE task_id = $1;

-- name: GetThreadOfMessage :one
SELECT * FROM conversations WHERE parent_message_id = $1;

-- name: GetAccessRoot :one
-- The conversation that decides who may read this one: a thread is read by whoever may read the
-- conversation its root message lives in. Threads do not nest, so one step is enough.
SELECT r.* FROM conversations c
LEFT JOIN messages pm ON pm.id = c.parent_message_id
JOIN conversations r ON r.id = COALESCE(pm.conversation_id, c.id)
WHERE c.id = $1;

-- name: UpdateConversation :one
UPDATE conversations
SET name = COALESCE(sqlc.narg(name), name),
    topic = COALESCE(sqlc.narg(topic), topic),
    archived_at = CASE WHEN sqlc.narg(archived)::bool IS NULL THEN archived_at
                       WHEN sqlc.narg(archived)::bool THEN COALESCE(archived_at, now())
                       ELSE NULL END
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: ListProjectConversations :many
-- A project's conversations of one kind as one person sees the list: those open to the project
-- and those they are in. The caller still checks each with the access rules (a task thread may
-- hang under a private channel).
SELECT sqlc.embed(c),
       (cm.member_id IS NOT NULL)::bool AS joined,
       COALESCE(cm.last_read_seq, 0)::bigint AS last_read_seq,
       COALESCE(cm.notify, '')::text AS notify,
       (SELECT count(*) FROM message_mentions mm JOIN messages m ON m.id = mm.message_id
        WHERE m.conversation_id = c.id AND mm.member_kind = 'user' AND mm.member_id = sqlc.arg(user_id)
          AND m.seq > COALESCE(cm.last_read_seq, 0) AND m.deleted_at IS NULL)::bigint AS unread_mentions
FROM conversations c
LEFT JOIN conversation_members cm
       ON cm.conversation_id = c.id AND cm.member_kind = 'user' AND cm.member_id = sqlc.arg(user_id)
WHERE c.project_id = sqlc.arg(project_id) AND c.kind = sqlc.arg(kind)
  AND (sqlc.arg(include_archived)::bool OR c.archived_at IS NULL)
  AND (sqlc.narg(name_like)::text IS NULL OR c.name ILIKE sqlc.narg(name_like)::text)
  AND (cm.member_id IS NOT NULL OR (c.visibility = 'project' AND c.kind <> 'dm'))
ORDER BY c.name NULLS LAST, c.last_message_at DESC NULLS LAST
LIMIT 500;

-- name: ListMemberConversations :many
-- Every conversation a person is in, across projects, newest activity first: the sidebar's
-- unread counts and DMs outside any project.
SELECT sqlc.embed(c),
       true::bool AS joined,
       cm.last_read_seq::bigint AS last_read_seq,
       cm.notify::text AS notify,
       (SELECT count(*) FROM message_mentions mm JOIN messages m ON m.id = mm.message_id
        WHERE m.conversation_id = c.id AND mm.member_kind = 'user' AND mm.member_id = sqlc.arg(user_id)
          AND m.seq > cm.last_read_seq AND m.deleted_at IS NULL)::bigint AS unread_mentions
FROM conversation_members cm
JOIN conversations c ON c.id = cm.conversation_id
WHERE cm.member_kind = 'user' AND cm.member_id = sqlc.arg(user_id) AND c.archived_at IS NULL
  AND (sqlc.narg(kind)::text IS NULL OR c.kind = sqlc.narg(kind)::text)
ORDER BY c.last_message_at DESC NULLS LAST, c.created_at DESC
LIMIT 500;

-- name: NextMessageSeq :one
-- Takes the next seq and holds the conversation's row lock until the transaction ends, so
-- concurrent posts are numbered one after another without gaps.
UPDATE conversations SET last_seq = last_seq + 1, last_message_at = now()
WHERE id = $1
RETURNING last_seq;

-- name: LinkTaskConversation :one
-- A thread becomes a task's thread when the message it hangs under is made into a task.
UPDATE conversations SET kind = 'task', task_id = sqlc.arg(task_id), project_id = sqlc.arg(project_id)
WHERE id = sqlc.arg(id) AND kind = 'thread'
RETURNING *;

-- name: AddConversationMember :one
-- Nothing comes back when the member is already in: joining twice changes nothing.
INSERT INTO conversation_members (conversation_id, member_kind, member_id, role, notify)
VALUES (sqlc.arg(conversation_id), sqlc.arg(member_kind), sqlc.arg(member_id), sqlc.arg(role), sqlc.arg(notify))
ON CONFLICT (conversation_id, member_kind, member_id) DO NOTHING
RETURNING *;

-- name: GetConversationMember :one
SELECT * FROM conversation_members WHERE conversation_id = $1 AND member_kind = $2 AND member_id = $3;

-- name: UpdateConversationMember :one
UPDATE conversation_members
SET notify = COALESCE(sqlc.narg(notify), notify), role = COALESCE(sqlc.narg(role), role)
WHERE conversation_id = sqlc.arg(conversation_id) AND member_kind = sqlc.arg(member_kind) AND member_id = sqlc.arg(member_id)
RETURNING *;

-- name: RemoveConversationMember :execrows
DELETE FROM conversation_members WHERE conversation_id = $1 AND member_kind = $2 AND member_id = $3;

-- name: ListConversationMembers :many
SELECT cm.*, COALESCE(u.name, '')::text AS name
FROM conversation_members cm
LEFT JOIN users u ON cm.member_kind = 'user' AND u.id = cm.member_id
WHERE cm.conversation_id = $1
ORDER BY cm.joined_at, cm.member_id;

-- name: ListConversationUserIDs :many
SELECT member_id FROM conversation_members WHERE conversation_id = $1 AND member_kind = 'user';

-- name: MarkConversationRead :one
-- Reading never moves the marker back, and never past the newest message.
INSERT INTO conversation_members (conversation_id, member_kind, member_id, last_read_seq)
SELECT c.id, 'user', sqlc.arg(user_id), LEAST(sqlc.arg(seq)::bigint, c.last_seq)
FROM conversations c WHERE c.id = sqlc.arg(conversation_id)
ON CONFLICT (conversation_id, member_kind, member_id)
DO UPDATE SET last_read_seq = GREATEST(conversation_members.last_read_seq, EXCLUDED.last_read_seq)
RETURNING *;

-- name: JoinDefaultChannel :exec
-- A person who joins a project is in its #general.
INSERT INTO conversation_members (conversation_id, member_kind, member_id)
SELECT c.id, 'user', sqlc.arg(user_id) FROM conversations c
WHERE c.project_id = sqlc.arg(project_id) AND c.kind = 'channel' AND c.name = sqlc.arg(name)::text
ON CONFLICT DO NOTHING;

-- name: JoinAsAuthor :exec
-- Whoever posts is in the conversation from then on, and has read what they wrote.
INSERT INTO conversation_members (conversation_id, member_kind, member_id, last_read_seq)
VALUES (sqlc.arg(conversation_id), 'user', sqlc.arg(user_id), sqlc.arg(seq)::bigint)
ON CONFLICT (conversation_id, member_kind, member_id)
DO UPDATE SET last_read_seq = GREATEST(conversation_members.last_read_seq, EXCLUDED.last_read_seq);

-- name: LockConversation :exec
-- Holds the conversation's row until the transaction ends: writers of its cards go one at a time.
SELECT id FROM conversations WHERE id = $1 FOR UPDATE;
