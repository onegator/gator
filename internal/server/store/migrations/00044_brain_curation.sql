-- +goose Up
-- How far the curator has read each conversation: what a thread settled is proposed once, from
-- the messages after the last time, by one curation job at a time.
CREATE TABLE brain_curations (
    conversation_id uuid PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    -- The last seq a finished curation covered.
    upto_seq        bigint NOT NULL DEFAULT 0,
    -- The curation being written: the job, and the seq it covers.
    job_id          uuid REFERENCES jobs(id) ON DELETE SET NULL,
    pending_upto    bigint,
    claimed_at      timestamptz,
    curated_at      timestamptz,
    -- A curation that failed is tried again later, later each time.
    attempts        integer NOT NULL DEFAULT 0,
    next_at         timestamptz
);
CREATE INDEX brain_curations_job_idx ON brain_curations (job_id) WHERE job_id IS NOT NULL;

-- +goose Down
DROP TABLE brain_curations;
