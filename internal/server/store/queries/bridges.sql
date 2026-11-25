-- name: UpsertExternalIdentity :one
-- Who said something on a bridge: the display name follows the outside, the link does not.
INSERT INTO external_identities (project_id, provider, external_id, display_name)
VALUES (sqlc.arg(project_id), sqlc.arg(provider), sqlc.arg(external_id), sqlc.arg(display_name))
ON CONFLICT (project_id, provider, external_id)
DO UPDATE SET display_name = CASE WHEN EXCLUDED.display_name = '' THEN external_identities.display_name ELSE EXCLUDED.display_name END,
              updated_at = CASE WHEN EXCLUDED.display_name IN ('', external_identities.display_name) THEN external_identities.updated_at ELSE now() END
RETURNING *;

-- name: GetExternalIdentity :one
SELECT * FROM external_identities WHERE id = $1;

-- name: FindExternalIdentity :one
SELECT * FROM external_identities
WHERE project_id = sqlc.arg(project_id) AND provider = sqlc.arg(provider) AND external_id = sqlc.arg(external_id);

-- name: ListExternalIdentities :many
SELECT * FROM external_identities WHERE project_id = $1 ORDER BY provider, display_name, external_id;

-- name: LinkExternalIdentity :one
UPDATE external_identities SET user_id = sqlc.arg(user_id), linked_at = now(), updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: UnlinkExternalIdentity :one
UPDATE external_identities SET user_id = NULL, linked_at = NULL, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: InsertLinkCode :exec
INSERT INTO external_link_codes (hash, project_id, provider, user_id, expires_at)
VALUES (sqlc.arg(hash), sqlc.arg(project_id), sqlc.arg(provider), sqlc.arg(user_id), sqlc.arg(expires_at));

-- name: UseLinkCode :one
-- A code works once, before it expires, in the project and for the provider it was made for.
UPDATE external_link_codes SET used_at = now()
WHERE hash = sqlc.arg(hash) AND project_id = sqlc.arg(project_id) AND provider = sqlc.arg(provider)
  AND used_at IS NULL AND expires_at > now()
RETURNING *;

-- name: DeleteExpiredLinkCodes :execrows
DELETE FROM external_link_codes WHERE expires_at < now() - interval '1 day';

-- name: CountRecentLinkCodes :one
SELECT count(*) FROM external_link_codes
WHERE user_id = sqlc.arg(user_id) AND created_at > sqlc.arg(since);

-- name: GetExternalConversation :one
SELECT * FROM external_conversation_refs
WHERE project_id = sqlc.arg(project_id) AND provider = sqlc.arg(provider) AND external_ref = sqlc.arg(external_ref);

-- name: GetConversationRefOf :one
-- The outside home of a conversation on one provider.
SELECT * FROM external_conversation_refs WHERE provider = sqlc.arg(provider) AND conversation_id = sqlc.arg(conversation_id);

-- name: InsertExternalConversation :one
INSERT INTO external_conversation_refs (project_id, provider, external_ref, conversation_id)
VALUES (sqlc.arg(project_id), sqlc.arg(provider), sqlc.arg(external_ref), sqlc.arg(conversation_id))
ON CONFLICT DO NOTHING
RETURNING *;

-- name: ListBridgesOfConversation :many
-- The providers a conversation is bridged to, itself or through the conversation its root lives in.
SELECT r.* FROM external_conversation_refs r
WHERE r.conversation_id = sqlc.arg(conversation_id) OR r.conversation_id = sqlc.arg(root_id);

-- name: GetExternalMessage :one
SELECT * FROM external_message_refs
WHERE project_id = sqlc.arg(project_id) AND provider = sqlc.arg(provider) AND external_ref = sqlc.arg(external_ref);

-- name: GetMessageRefOf :one
SELECT * FROM external_message_refs WHERE provider = sqlc.arg(provider) AND message_id = sqlc.arg(message_id);

-- name: InsertExternalMessage :one
INSERT INTO external_message_refs (project_id, provider, external_ref, message_id)
VALUES (sqlc.arg(project_id), sqlc.arg(provider), sqlc.arg(external_ref), sqlc.arg(message_id))
ON CONFLICT DO NOTHING
RETURNING *;
