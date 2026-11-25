-- name: InsertCheckpoint :one
-- A checkpoint sent again (a runner resends what was not acked) is the one already there.
INSERT INTO turn_checkpoints (job_id, turn_id, seq, repo, base_commit, head_commit, files, diff_key, diff, diff_bytes, truncated,
                              whole_files, whole_key, whole_diff, whole_bytes, whole_truncated, took_ms)
VALUES (sqlc.arg(job_id), sqlc.narg(turn_id), sqlc.arg(seq), sqlc.arg(repo), sqlc.arg(base_commit), sqlc.arg(head_commit),
        sqlc.arg(files), sqlc.narg(diff_key), sqlc.narg(diff), sqlc.arg(diff_bytes), sqlc.arg(truncated),
        sqlc.arg(whole_files), sqlc.narg(whole_key), sqlc.narg(whole_diff), sqlc.arg(whole_bytes), sqlc.arg(whole_truncated),
        sqlc.arg(took_ms))
ON CONFLICT (job_id, repo, seq) DO UPDATE SET job_id = EXCLUDED.job_id
RETURNING id, (xmax = 0)::boolean AS inserted;

-- name: ListCheckpoints :many
SELECT id, job_id, turn_id, seq, repo, base_commit, head_commit, files, diff_bytes, truncated, whole_files, whole_bytes,
       whole_truncated, took_ms, created_at
FROM turn_checkpoints WHERE job_id = $1 ORDER BY repo, seq;

-- name: GetCheckpoint :one
SELECT id, job_id, turn_id, seq, repo, base_commit, head_commit, files, diff_bytes, truncated, whole_files, whole_bytes,
       whole_truncated, took_ms, created_at
FROM turn_checkpoints WHERE id = $1;

-- name: CheckpointDiff :one
-- Where a checkpoint's diffs are: a storage key, or the bytes themselves.
SELECT diff_key, diff, whole_key, whole_diff FROM turn_checkpoints WHERE id = $1;

-- name: JobChanges :one
-- What a job changed so far, for its card: the files of its latest checkpoint's diff of the whole job.
SELECT COALESCE((SELECT whole_files FROM turn_checkpoints c WHERE c.job_id = $1 ORDER BY c.seq DESC, c.repo LIMIT 1), '[]'::jsonb)::jsonb AS files;

-- name: InsertReviewComment :one
INSERT INTO review_comments (checkpoint_id, author_id, path, side, line_start, line_end, body, snippet)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetReviewComment :one
SELECT * FROM review_comments WHERE id = $1;

-- name: GetReviewCommentForUpdate :one
SELECT * FROM review_comments WHERE id = $1 FOR UPDATE;

-- name: ListJobReviewComments :many
-- A job's comments: those sent, and the drafts of the one asking.
SELECT rc.*, c.seq AS checkpoint_seq FROM review_comments rc
JOIN turn_checkpoints c ON c.id = rc.checkpoint_id
WHERE c.job_id = sqlc.arg(job_id) AND (rc.message_id IS NOT NULL OR rc.author_id = sqlc.arg(viewer))
ORDER BY c.seq, rc.path, rc.line_start, rc.created_at;

-- name: ListDraftReviewComments :many
-- An author's drafts on a job, locked while they are sent.
SELECT rc.* FROM review_comments rc
JOIN turn_checkpoints c ON c.id = rc.checkpoint_id
WHERE c.job_id = sqlc.arg(job_id) AND rc.author_id = sqlc.arg(author_id) AND rc.message_id IS NULL AND rc.resolved_at IS NULL
ORDER BY c.seq, rc.path, rc.line_start, rc.created_at
FOR UPDATE OF rc;

-- name: SendReviewComments :exec
UPDATE review_comments SET message_id = sqlc.arg(message_id), updated_at = now()
WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND message_id IS NULL;

-- name: UpdateReviewComment :one
UPDATE review_comments SET body = sqlc.arg(body),
    resolved_at = CASE WHEN sqlc.arg(resolved)::boolean THEN COALESCE(resolved_at, now()) ELSE NULL END, updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteReviewComment :exec
DELETE FROM review_comments WHERE id = $1 AND message_id IS NULL;

-- name: ListReviewNotes :many
-- The comments sent with these messages, for the agent who reads them: the file, the lines, the
-- code they are about and what was said, by whom.
SELECT rc.message_id, rc.path, rc.side, rc.line_start, rc.line_end, rc.snippet, rc.body, u.name AS author
FROM review_comments rc JOIN users u ON u.id = rc.author_id
WHERE rc.message_id = ANY(sqlc.arg(message_ids)::uuid[])
ORDER BY rc.message_id, rc.path, rc.line_start, rc.created_at;
