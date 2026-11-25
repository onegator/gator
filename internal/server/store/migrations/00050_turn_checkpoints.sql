-- +goose Up
-- What each turn of a job changed: the runner checkpoints the worktree after every turn (a commit
-- on a hidden ref that is never pushed) and sends the turn's diff and the job's diff so far. The
-- diffs live with attachments (a bucket or a directory: *_key) or, on a server with neither, in
-- the row (diff, whole_diff). Only redacted text is ever stored.

CREATE TABLE turn_checkpoints (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id          uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    turn_id         uuid REFERENCES agent_turns(id) ON DELETE SET NULL,
    attempt         integer NOT NULL DEFAULT 1,          -- a job leased again starts its turns over
    seq             integer NOT NULL CHECK (seq >= 1),   -- the turn of the job: 1 is the first
    repo            text NOT NULL,                       -- projects with more than one repository
    base_commit     text NOT NULL,
    head_commit     text NOT NULL,
    files           jsonb NOT NULL,                      -- [{path, added, removed, status, binary}]
    files_truncated boolean NOT NULL DEFAULT false,      -- more files changed than are listed
    diff_key        text,
    diff            bytea,
    diff_bytes      bigint NOT NULL DEFAULT 0,
    truncated       boolean NOT NULL DEFAULT false,
    -- The job so far: from the worktree as the job found it to this turn.
    whole_files     jsonb NOT NULL DEFAULT '[]',
    whole_key       text,
    whole_diff      bytea,
    whole_bytes     bigint NOT NULL DEFAULT 0,
    whole_truncated boolean NOT NULL DEFAULT false,
    whole_files_truncated boolean NOT NULL DEFAULT false,
    took_ms         integer NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (job_id, attempt, repo, seq)
);
CREATE INDEX turn_checkpoints_turn_idx ON turn_checkpoints (turn_id) WHERE turn_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS turn_checkpoints;
