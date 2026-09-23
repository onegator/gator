-- +goose Up
-- What an agent said back to Gator while it worked. Plugins have had an audit since M3; agents
-- had nothing, because they could not talk. Kept beside the job so the work and the words about
-- it are read together.
CREATE TABLE agent_calls (
    id         bigserial PRIMARY KEY,
    job_id     uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    task_id    uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    command    text NOT NULL,
    detail     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX agent_calls_job_idx ON agent_calls (job_id, id);

-- A gate can now say an agent stopped it, not only a person or the automation.
ALTER TABLE gates DROP CONSTRAINT gates_blocked_by_check;
ALTER TABLE gates ADD CONSTRAINT gates_blocked_by_check CHECK (blocked_by IN ('automation', 'human', 'agent'));

-- +goose Down
ALTER TABLE gates DROP CONSTRAINT gates_blocked_by_check;
ALTER TABLE gates ADD CONSTRAINT gates_blocked_by_check CHECK (blocked_by IN ('automation', 'human'));
DROP TABLE agent_calls;
