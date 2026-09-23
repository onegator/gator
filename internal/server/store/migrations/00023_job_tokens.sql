-- +goose Up
-- A token that belongs to one job. An agent needs to talk back to Gator — to say it is
-- blocked, to record what it decided — and until now the only identities were a person, a
-- runner and a plugin. Handing an agent the runner's token would give the words of one task
-- authority over every other; this one dies with its job.
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_kind_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_kind_check CHECK (kind IN ('user', 'runner', 'plugin', 'job'));
ALTER TABLE api_tokens ADD COLUMN job_id uuid REFERENCES jobs(id) ON DELETE CASCADE;

CREATE INDEX api_tokens_job_idx ON api_tokens (job_id) WHERE job_id IS NOT NULL;

-- +goose Down
DROP INDEX api_tokens_job_idx;
ALTER TABLE api_tokens DROP COLUMN job_id;
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_kind_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_kind_check CHECK (kind IN ('user', 'runner', 'plugin'));
