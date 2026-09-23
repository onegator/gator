-- +goose Up
-- The package of documents a job was handed, as it was handed over. We counted its size from
-- the start, which answers "how much" but never "what" — and never why something a person
-- expected to be there was not. Kept per job, because the answer changes as knowledge and the
-- product context change, and the question is always about the job that already ran.
CREATE TABLE job_context (
    job_id     uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    position   integer NOT NULL,
    kind       text NOT NULL,
    phase      text NOT NULL DEFAULT '',
    title      text NOT NULL,
    -- Where it came from, in the words a person uses: task, catalogue, product, knowledge,
    -- artifact, plugin.
    origin     text NOT NULL DEFAULT '',
    -- The document as it went into the prompt; empty when it was left out entirely.
    body       text NOT NULL DEFAULT '',
    full_bytes integer NOT NULL DEFAULT 0,
    -- Why it did not arrive whole, in a sentence. Empty when it did.
    left_out   text NOT NULL DEFAULT '',
    dropped    boolean NOT NULL DEFAULT false,
    PRIMARY KEY (job_id, position)
);

-- +goose Down
DROP TABLE job_context;
