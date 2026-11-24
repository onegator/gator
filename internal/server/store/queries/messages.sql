-- name: InsertMessage :one
INSERT INTO messages (conversation_id, seq, author_kind, author_id, kind, body, card_type, card, card_key, client_id, origin, status)
VALUES (sqlc.arg(conversation_id), sqlc.arg(seq), sqlc.arg(author_kind), sqlc.narg(author_id), sqlc.arg(kind), sqlc.arg(body),
        sqlc.narg(card_type), sqlc.narg(card), sqlc.narg(card_key), sqlc.narg(client_id), sqlc.arg(origin), sqlc.arg(status))
RETURNING *;

-- name: GetMessage :one
SELECT * FROM messages WHERE id = $1;

-- name: GetMessageByClientID :one
-- A client id is the sender's own: another author's message with the same one is not this.
SELECT * FROM messages
WHERE conversation_id = $1 AND author_kind = $2 AND author_id = $3 AND client_id = $4;

-- name: ListMessagesBefore :many
-- The newest messages below a seq, newest first; the caller puts them back in order.
SELECT * FROM messages
WHERE conversation_id = sqlc.arg(conversation_id) AND seq < sqlc.arg(before_seq)
ORDER BY seq DESC
LIMIT sqlc.arg(max_rows);

-- name: ListMessagesAfter :many
SELECT * FROM messages
WHERE conversation_id = sqlc.arg(conversation_id) AND seq > sqlc.arg(after_seq)
ORDER BY seq
LIMIT sqlc.arg(max_rows);

-- name: IncrementReplyCount :one
UPDATE messages SET reply_count = reply_count + 1 WHERE id = $1
RETURNING *;

-- name: ListThreadsOfMessages :many
-- The thread under each of these messages, where there is one.
SELECT parent_message_id, id FROM conversations WHERE parent_message_id = ANY(sqlc.arg(message_ids)::uuid[]);

-- name: AddMention :exec
INSERT INTO message_mentions (message_id, member_kind, member_id) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: ListMentionsOfMessages :many
SELECT * FROM message_mentions WHERE message_id = ANY(sqlc.arg(message_ids)::uuid[])
ORDER BY message_id, member_kind, member_id;

-- name: GetCardForUpdate :one
-- The card about one thing in a conversation, locked while it is brought up to date.
SELECT * FROM messages WHERE conversation_id = $1 AND card_key = $2 FOR UPDATE;

-- name: UpdateCard :one
UPDATE messages SET body = sqlc.arg(body), card = sqlc.arg(card), edited_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: FindCard :one
-- A card by what it is about, in any of these conversations (a task's card sits in its channel,
-- or in the task's own thread when that thread was there first).
SELECT * FROM messages WHERE card_key = sqlc.arg(card_key) AND conversation_id = ANY(sqlc.arg(conversation_ids)::uuid[])
ORDER BY created_at LIMIT 1;

-- name: ListCardsOfType :many
SELECT * FROM messages WHERE conversation_id = $1 AND card_type = $2 AND kind = 'card' ORDER BY seq;

-- name: EditMessage :one
UPDATE messages SET body = sqlc.arg(body), edited_at = now()
WHERE id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteMessage :one
-- A deleted message keeps its place (its seq, its thread) and loses its words.
UPDATE messages SET deleted_at = now(), body = ''
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: AddReaction :execrows
INSERT INTO message_reactions (message_id, member_kind, member_id, emoji) VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: RemoveReaction :execrows
DELETE FROM message_reactions WHERE message_id = $1 AND member_kind = $2 AND member_id = $3 AND emoji = $4;

-- name: ListReactionsOfMessages :many
SELECT * FROM message_reactions WHERE message_id = ANY(sqlc.arg(message_ids)::uuid[])
ORDER BY message_id, emoji, created_at;

-- name: CreateAttachment :one
INSERT INTO message_attachments (conversation_id, uploaded_by, name, content_type, size_bytes, storage_key)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: AttachToMessage :execrows
-- Only files the author uploaded into this conversation and has not sent yet.
UPDATE message_attachments SET message_id = sqlc.arg(message_id)
WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND conversation_id = sqlc.arg(conversation_id)
  AND uploaded_by = sqlc.arg(uploaded_by) AND message_id IS NULL;

-- name: GetAttachment :one
SELECT * FROM message_attachments WHERE id = $1;

-- name: ListAttachmentsOfMessages :many
SELECT * FROM message_attachments WHERE message_id = ANY(sqlc.arg(message_ids)::uuid[])
ORDER BY message_id, created_at;

-- name: ListThreadParticipants :many
-- The people a thread is theirs: whoever wrote its root message or a reply in it.
SELECT DISTINCT author_id FROM messages
WHERE (conversation_id = sqlc.arg(conversation_id) OR id = sqlc.arg(root_id))
  AND author_kind = 'user' AND author_id IS NOT NULL AND deleted_at IS NULL;
