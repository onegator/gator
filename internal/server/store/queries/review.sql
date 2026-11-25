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
