-- name: SearchMessages :many
-- Messages matching a query, best first, among the conversations the caller could read: open
-- conversations of the projects they read, and any they are in (or whose root they are in). The
-- caller still passes every hit through the access rules; this only keeps the candidates few.
SELECT sqlc.embed(m)
FROM messages m
JOIN conversations c ON c.id = m.conversation_id
LEFT JOIN messages pm ON pm.id = c.parent_message_id
JOIN conversations r ON r.id = COALESCE(pm.conversation_id, c.id)
WHERE m.deleted_at IS NULL
  AND to_tsvector('simple', m.body) @@ websearch_to_tsquery('simple', sqlc.arg(query)::text)
  AND (sqlc.narg(project_id)::uuid IS NULL OR c.project_id = sqlc.narg(project_id)::uuid)
  AND (
        (r.kind <> 'dm' AND r.visibility = 'project'
         AND (sqlc.arg(all_projects)::bool OR r.project_id = ANY(sqlc.arg(project_ids)::uuid[])))
     OR EXISTS (SELECT 1 FROM conversation_members cm
                WHERE cm.conversation_id = r.id AND cm.member_kind = 'user' AND cm.member_id = sqlc.arg(user_id)::uuid)
  )
ORDER BY ts_rank(to_tsvector('simple', m.body), websearch_to_tsquery('simple', sqlc.arg(query)::text)) DESC, m.created_at DESC
LIMIT sqlc.arg(max_rows) OFFSET sqlc.arg(skip);
