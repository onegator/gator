-- name: InsertMessage :one
INSERT INTO messages (conversation_id, seq, author_kind, author_id, kind, body, card_type, card, card_key, client_id, origin, status)
VALUES (sqlc.arg(conversation_id), sqlc.arg(seq), sqlc.arg(author_kind), sqlc.narg(author_id), sqlc.arg(kind), sqlc.arg(body),
        sqlc.narg(card_type), sqlc.narg(card), sqlc.narg(card_key), sqlc.narg(client_id), sqlc.arg(origin), sqlc.arg(status))
RETURNING *;

-- name: GetMessage :one
SELECT * FROM messages WHERE id = $1;

-- name: GetMessageByClientID :one
SELECT * FROM messages WHERE conversation_id = $1 AND client_id = $2;

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
