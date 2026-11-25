-- name: InsertBrainEntry :one
INSERT INTO brain_entries (
    scope, project_id, component_key, user_id, agent_id, kind, topic, title, content, visibility,
    status, lineage, version, supersedes, confidence, source, source_task_id, source_conversation_id,
    source_private, position, condensed_from, created_by_kind, created_by, approved_by, approved_at)
VALUES (
    sqlc.arg(scope), sqlc.narg(project_id), sqlc.narg(component_key), sqlc.narg(user_id), sqlc.narg(agent_id),
    sqlc.arg(kind), sqlc.narg(topic), sqlc.arg(title), sqlc.arg(content), sqlc.arg(visibility),
    sqlc.arg(status), sqlc.narg(lineage), sqlc.arg(version), sqlc.narg(supersedes), sqlc.arg(confidence),
    sqlc.arg(source), sqlc.narg(source_task_id), sqlc.narg(source_conversation_id),
    sqlc.arg(source_private), sqlc.arg(position), sqlc.arg(condensed_from), sqlc.arg(created_by_kind),
    sqlc.narg(created_by), sqlc.narg(approved_by), sqlc.narg(approved_at))
RETURNING *;

-- name: GetBrainEntry :one
SELECT * FROM brain_entries WHERE id = $1;

-- name: LatestBrainVersionByTitle :one
-- The newest version of the entry with this title in this place: writing the same title again is
-- a new version of it, as product_context always worked. Conventions are not versioned by title.
SELECT * FROM brain_entries
WHERE scope = sqlc.arg(scope)
  AND project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::uuid
  AND component_key IS NOT DISTINCT FROM sqlc.narg(component_key)::text
  AND user_id IS NOT DISTINCT FROM sqlc.narg(user_id)::uuid
  AND agent_id IS NOT DISTINCT FROM sqlc.narg(agent_id)::uuid
  AND kind = sqlc.arg(kind) AND title = sqlc.arg(title) AND topic IS NULL
ORDER BY version DESC LIMIT 1;

-- name: LatestBrainVersion :one
SELECT * FROM brain_entries WHERE lineage = $1 ORDER BY version DESC LIMIT 1;

-- name: ListBrainLineage :many
-- Every version of one entry, newest first: its history.
SELECT * FROM brain_entries WHERE lineage = $1 ORDER BY version DESC;

-- name: SetBrainStatus :one
UPDATE brain_entries SET status = sqlc.arg(status),
    approved_by = CASE WHEN sqlc.arg(status)::text = 'approved' THEN sqlc.narg(approved_by)::uuid ELSE approved_by END,
    approved_at = CASE WHEN sqlc.arg(status)::text = 'approved' THEN now() ELSE approved_at END,
    valid_from  = CASE WHEN sqlc.arg(status)::text = 'approved' THEN now() ELSE valid_from END,
    valid_until = CASE WHEN sqlc.arg(status)::text IN ('superseded', 'archived') THEN coalesce(valid_until, now()) ELSE valid_until END,
    updated_at = now()
WHERE id = sqlc.arg(id) RETURNING *;

-- name: SupersedeOtherVersions :execrows
-- Once a version is approved, the versions before it stop being true from now on. They stay, so
-- "what did we know in March" has an answer.
UPDATE brain_entries SET status = 'superseded', valid_until = now(), updated_at = now()
WHERE lineage = sqlc.arg(lineage) AND id <> sqlc.arg(id) AND status = 'approved';

-- name: SupersedeBrainEntry :one
UPDATE brain_entries SET status = 'superseded', valid_until = now(), updated_at = now()
WHERE id = $1 AND status = 'approved' RETURNING *;

-- name: ArchiveBrainEntries :execrows
UPDATE brain_entries SET archived_at = now(), updated_at = now()
WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND archived_at IS NULL;

-- name: DeleteBrainEntry :execrows
DELETE FROM brain_entries WHERE id = $1;

-- name: SearchBrain :many
-- Candidates for a reader, by words (websearch syntax) or, without words, the newest first. The
-- reach is wider than the rules, never narrower: every row still passes brain.Policy.
SELECT sqlc.embed(e), (CASE WHEN sqlc.arg(query)::text = '' THEN 0
             ELSE ts_rank(to_tsvector('simple', e.title || ' ' || e.content), websearch_to_tsquery('simple', sqlc.arg(query)::text)) END)::real AS rank
FROM brain_entries e
WHERE (sqlc.arg(query)::text = '' OR to_tsvector('simple', e.title || ' ' || e.content) @@ websearch_to_tsquery('simple', sqlc.arg(query)::text))
  AND (cardinality(sqlc.arg(scopes)::text[]) = 0 OR e.scope = ANY(sqlc.arg(scopes)::text[]))
  AND (cardinality(sqlc.arg(kinds)::text[]) = 0 OR e.kind = ANY(sqlc.arg(kinds)::text[]))
  AND (cardinality(sqlc.arg(statuses)::text[]) = 0 OR e.status = ANY(sqlc.arg(statuses)::text[]))
  AND (sqlc.narg(project_id)::uuid IS NULL OR e.project_id = sqlc.narg(project_id)::uuid OR e.project_id IS NULL)
  AND (sqlc.arg(include_archived)::boolean OR e.archived_at IS NULL)
  -- What held at a moment: approved by then, and not yet replaced or archived.
  AND (sqlc.narg(at)::timestamptz IS NULL OR (e.status IN ('approved', 'superseded', 'archived')
       AND e.approved_at <= sqlc.narg(at)::timestamptz AND e.valid_from <= sqlc.narg(at)::timestamptz
       AND (e.valid_until IS NULL OR e.valid_until > sqlc.narg(at)::timestamptz)))
  AND (e.scope = 'workspace' OR e.visibility = 'workspace'
       OR (e.scope IN ('project', 'component', 'agent') AND (sqlc.arg(every_project)::boolean OR e.project_id = ANY(sqlc.arg(projects)::uuid[])))
       OR (e.scope = 'user' AND (e.user_id = sqlc.narg(user_id)::uuid
                                 OR (e.visibility = 'project' AND (sqlc.arg(every_project)::boolean OR e.project_id = ANY(sqlc.arg(projects)::uuid[]))))))
ORDER BY rank DESC, e.updated_at DESC, e.id
LIMIT sqlc.arg(max_rows);

-- name: LiveBrainEntries :many
-- What may reach a prompt: the newest approved version of every entry, not folded into a
-- condensed one and still true. Narrowed by scope and place; brain.Policy decides the rest.
SELECT DISTINCT ON (lineage) * FROM brain_entries
WHERE status = 'approved' AND archived_at IS NULL AND (valid_until IS NULL OR valid_until > now())
  AND scope = ANY(sqlc.arg(scopes)::text[])
  AND (sqlc.narg(project_id)::uuid IS NULL OR project_id = sqlc.narg(project_id)::uuid OR project_id IS NULL)
ORDER BY lineage, version DESC;

-- name: ListBrainProposals :many
-- What waits for a person, in live projects (and the workspace's own).
SELECT e.* FROM brain_entries e LEFT JOIN projects p ON p.id = e.project_id
WHERE e.status = 'proposed' AND e.archived_at IS NULL AND (e.project_id IS NULL OR p.archived_at IS NULL)
  AND (sqlc.narg(project_id)::uuid IS NULL OR e.project_id = sqlc.narg(project_id)::uuid)
ORDER BY e.created_at, e.id;

-- name: CountBrainProposalsForTask :one
-- Keeps a repeated event from proposing the same thing twice.
SELECT count(*) FROM brain_entries WHERE project_id = $1 AND kind = $2 AND source_task_id = $3;

-- name: CountBrainFromSource :one
-- Whether something already came from this source (a message, a plugin's document), so it is
-- proposed once.
SELECT count(*) FROM brain_entries
WHERE source->>'kind' = sqlc.arg(source_kind)::text AND source->>'id' = sqlc.arg(source_id)::text
  AND project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::uuid;

-- name: ListUserBrain :many
-- Everything about one person, every version: what "me" is, for export and for the screen.
SELECT * FROM brain_entries WHERE scope = 'user' AND user_id = $1 ORDER BY kind, title, version DESC;

-- name: DeleteUserBrain :execrows
DELETE FROM brain_entries WHERE scope = 'user' AND user_id = $1;

-- name: InsertBrainLink :exec
INSERT INTO brain_links (entry_id, target_kind, target_id, relation) VALUES ($1, $2, $3, $4)
ON CONFLICT (entry_id, target_kind, target_id) DO UPDATE SET relation = EXCLUDED.relation;

-- name: ListBrainLinks :many
SELECT * FROM brain_links WHERE entry_id = ANY(sqlc.arg(ids)::uuid[]) ORDER BY entry_id, target_kind, target_id;

-- name: DeleteBrainLink :exec
DELETE FROM brain_links WHERE entry_id = $1 AND target_kind = $2 AND target_id = $3;

-- name: RecordBrainCitations :exec
INSERT INTO brain_citations (job_id, entry_id, how)
SELECT sqlc.arg(job_id)::uuid, unnest(sqlc.arg(entry_ids)::uuid[]), sqlc.arg(how)::text
ON CONFLICT DO NOTHING;

-- name: ListBrainCitations :many
-- Where an entry was used: the jobs that were handed it, looked it up or cited it.
SELECT c.job_id, c.entry_id, c.how, c.created_at, j.task_id, j.role FROM brain_citations c JOIN jobs j ON j.id = c.job_id
WHERE c.entry_id = ANY(sqlc.arg(ids)::uuid[]) ORDER BY c.created_at DESC LIMIT 200;

-- name: ListJobCitations :many
SELECT * FROM brain_citations WHERE job_id = $1 ORDER BY how, entry_id;

-- name: StaleBrainEntries :many
-- Approved entries nobody has cited since before: the ones worth asking "is this still true?".
SELECT e.* FROM brain_entries e
WHERE e.status = 'approved' AND e.archived_at IS NULL AND e.approved_at < sqlc.arg(before)::timestamptz
  AND (sqlc.narg(project_id)::uuid IS NULL OR e.project_id = sqlc.narg(project_id)::uuid)
  AND NOT EXISTS (SELECT 1 FROM brain_citations c WHERE c.entry_id = e.id AND c.how = 'cited' AND c.created_at >= sqlc.arg(before)::timestamptz)
ORDER BY e.approved_at LIMIT 100;

-- The doors the product context and the knowledge entries had, over the Brain.

-- name: ListProductEntries :many
-- A project's product context, every version: what GET /projects/{id}/product always answered.
SELECT * FROM brain_entries
WHERE scope = 'project' AND project_id = $1 AND kind = ANY(sqlc.arg(kinds)::text[])
ORDER BY kind, title, version DESC;

-- name: ListProposedProduct :many
-- What the inbox shows of the product context: proposals waiting for a person, in live projects.
SELECT e.* FROM brain_entries e JOIN projects p ON p.id = e.project_id
WHERE e.scope = 'project' AND e.kind = ANY(sqlc.arg(kinds)::text[]) AND e.status = 'proposed' AND e.archived_at IS NULL
  AND p.archived_at IS NULL AND (sqlc.narg(project_id)::uuid IS NULL OR e.project_id = sqlc.narg(project_id)::uuid)
ORDER BY e.created_at;

-- name: LiveEntriesOfKind :many
-- What a condensation would fold together: approved, not archived, not itself a condensate.
SELECT DISTINCT ON (lineage) * FROM brain_entries
WHERE scope = 'project' AND project_id = $1 AND kind = $2 AND status = 'approved' AND archived_at IS NULL
  AND cardinality(condensed_from) = 0
ORDER BY lineage, version DESC;

-- name: KindsWorthCondensing :many
-- Kinds whose live entries have grown past what a prompt should carry, with how much they hold.
SELECT project_id, kind, count(*)::bigint AS entries, coalesce(sum(length(content)), 0)::bigint AS bytes
FROM (SELECT DISTINCT ON (lineage) project_id, kind, content
      FROM brain_entries
      WHERE scope = 'project' AND kind = ANY(sqlc.arg(kinds)::text[]) AND status = 'approved'
        AND archived_at IS NULL AND cardinality(condensed_from) = 0
      ORDER BY lineage, version DESC) live
GROUP BY project_id, kind
HAVING count(*) >= sqlc.arg(min_entries)::bigint OR coalesce(sum(length(content)), 0) >= sqlc.arg(min_bytes)::bigint;

-- name: ListKnowledgeShelf :many
-- The conventions written by hand, on their shelves: a project's own, or the workspace's (no project).
SELECT * FROM brain_entries
WHERE kind = 'convention' AND topic IS NOT NULL AND status = 'approved' AND scope IN ('workspace', 'project')
  AND project_id IS NOT DISTINCT FROM sqlc.narg(project_id)::uuid
ORDER BY position, topic, title;

-- name: RecordCitedBrain :exec
-- What a job cites, of what it was handed or looked up: an agent cannot cite its way into an entry
-- it never read.
INSERT INTO brain_citations (job_id, entry_id, how)
SELECT sqlc.arg(job_id)::uuid, e, 'cited' FROM unnest(sqlc.arg(entry_ids)::uuid[]) e
WHERE EXISTS (SELECT 1 FROM brain_citations c WHERE c.job_id = sqlc.arg(job_id)::uuid AND c.entry_id = e AND c.how IN ('packed', 'searched'))
ON CONFLICT DO NOTHING;

-- name: ConversationsDueForCuration :many
-- Task threads that went quiet with enough said since their last curation, and no curation being
-- written (or one that has been for an hour, which is lost).
SELECT c.id, c.task_id, c.last_seq FROM conversations c
LEFT JOIN brain_curations b ON b.conversation_id = c.id
WHERE c.task_id IS NOT NULL AND c.archived_at IS NULL
  AND c.last_message_at < sqlc.arg(quiet_before)::timestamptz
  AND c.last_seq > coalesce(b.upto_seq, 0)
  AND (b.job_id IS NULL OR b.claimed_at < now() - interval '1 hour')
  AND (SELECT count(*) FROM messages m WHERE m.conversation_id = c.id AND m.seq > coalesce(b.upto_seq, 0)
       AND m.kind = 'text' AND m.author_kind IN ('user', 'agent') AND m.deleted_at IS NULL) >= sqlc.arg(min_messages)::bigint
ORDER BY c.last_message_at
LIMIT 50;

-- name: GetCuration :one
SELECT * FROM brain_curations WHERE conversation_id = $1;

-- name: ClaimCuration :one
-- Takes a conversation for one curation up to a seq, unless one is being written.
INSERT INTO brain_curations (conversation_id, pending_upto, claimed_at) VALUES ($1, sqlc.arg(upto)::bigint, now())
ON CONFLICT (conversation_id) DO UPDATE SET pending_upto = EXCLUDED.pending_upto, claimed_at = now(), job_id = NULL
WHERE brain_curations.job_id IS NULL OR brain_curations.claimed_at < now() - interval '1 hour'
RETURNING *;

-- name: SetCurationJob :exec
UPDATE brain_curations SET job_id = $2 WHERE conversation_id = $1;

-- name: CurationByJob :one
SELECT * FROM brain_curations WHERE job_id = $1;

-- name: FinishCuration :exec
UPDATE brain_curations SET upto_seq = greatest(upto_seq, coalesce(pending_upto, upto_seq)), pending_upto = NULL,
    job_id = NULL, curated_at = now()
WHERE conversation_id = $1;

-- name: ReleaseCuration :exec
UPDATE brain_curations SET job_id = NULL, pending_upto = NULL WHERE conversation_id = $1;

-- name: CountOpenProposalsFrom :one
-- Proposals from one conversation still waiting: a thread does not fill the inbox.
SELECT count(*) FROM brain_entries WHERE source_conversation_id = $1 AND status = 'proposed';

-- name: MessageAtSeq :one
SELECT * FROM messages WHERE conversation_id = $1 AND seq = $2;

-- name: TaskBrainOfDay :many
-- What the Brain was proposed from one task on one day, for the day's card in its thread.
SELECT * FROM brain_entries
WHERE source_task_id = $1 AND scope IN ('project', 'component') AND NOT source_private
  AND created_at >= sqlc.arg(day)::timestamptz AND created_at < sqlc.arg(day)::timestamptz + interval '1 day'
ORDER BY created_at, id;

-- name: ActiveTasksOf :many
-- The open tasks a person owns, or talked about in their thread since a moment: what their focus
-- is worked out from. Asks are a conversation's, not work.
SELECT t.id, t.project_id, t.title, t.phase, coalesce(t.owner_kind = 'user' AND t.owner_id = sqlc.arg(user_id)::uuid, false)::boolean AS owns
FROM tasks t
WHERE t.closed_at IS NULL AND t.kind <> 'ask' AND (
    (t.owner_kind = 'user' AND t.owner_id = sqlc.arg(user_id)::uuid)
    OR EXISTS (SELECT 1 FROM conversations c JOIN messages m ON m.conversation_id = c.id
               WHERE c.task_id = t.id AND m.author_kind = 'user' AND m.author_id = sqlc.arg(user_id)::uuid
                 AND m.created_at > sqlc.arg(since)::timestamptz))
ORDER BY t.project_id, t.title
LIMIT 100;

-- name: PeopleActiveSince :many
-- Who has worked or talked since a moment: whose focus is worth working out again.
SELECT DISTINCT m.author_id::uuid AS user_id FROM messages m
WHERE m.author_kind = 'user' AND m.author_id IS NOT NULL AND m.created_at > sqlc.arg(since)::timestamptz
UNION
SELECT DISTINCT t.owner_id::uuid FROM tasks t
WHERE t.owner_kind = 'user' AND t.owner_id IS NOT NULL AND t.updated_at > sqlc.arg(since)::timestamptz;

-- name: CurrentFocusOf :many
-- A person's focus as it stands, per project.
SELECT DISTINCT ON (lineage) * FROM brain_entries
WHERE scope = 'user' AND user_id = $1 AND kind = 'focus' AND status = 'approved' AND valid_until IS NULL
ORDER BY lineage, version DESC;
