-- +goose Up
-- One row per unit of agent work (a job, or a batch reported by a plugin or a person).
-- Time per task comes from phase_transitions; tokens, cost and agent time come from here.
CREATE TABLE usage_records (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id            uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    project_id         uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    phase              text NOT NULL,
    job_id             uuid,
    source             text NOT NULL CHECK (source IN ('runner', 'plugin', 'user', 'system')),
    actor_id           uuid,
    backend            text NOT NULL DEFAULT '',
    model              text NOT NULL DEFAULT '',
    input_tokens       bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens      bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cache_read_tokens  bigint NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    cache_write_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    cost_usd           double precision NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    cost_estimated     boolean NOT NULL DEFAULT true,
    duration_ms        bigint NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    started_at         timestamptz,
    finished_at        timestamptz,
    idempotency_key    text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    -- A retried report for the same task counts once; keys are scoped to the task so a
    -- client-chosen key can never collide with another task's. NULL keys never conflict.
    UNIQUE (task_id, idempotency_key)
);
CREATE INDEX usage_records_task_idx ON usage_records (task_id);
CREATE INDEX usage_records_project_created_idx ON usage_records (project_id, created_at);
CREATE TRIGGER usage_records_append_only BEFORE UPDATE OR DELETE ON usage_records
    FOR EACH ROW EXECUTE FUNCTION forbid_change();

-- +goose Down
DROP TRIGGER IF EXISTS usage_records_append_only ON usage_records;
DROP TABLE IF EXISTS usage_records;
