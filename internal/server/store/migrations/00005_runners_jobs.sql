-- +goose Up
-- One row per runner token: a runner's identity is the token it authenticates with.
CREATE TABLE runners (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_id          uuid NOT NULL UNIQUE REFERENCES api_tokens(id) ON DELETE CASCADE,
    name              text NOT NULL,
    location          text NOT NULL DEFAULT 'other' CHECK (location IN ('vps', 'mac', 'other')),
    status            text NOT NULL DEFAULT 'offline' CHECK (status IN ('online', 'busy', 'offline')),
    capabilities      jsonb NOT NULL DEFAULT '{}'::jsonb,
    auth_state        jsonb NOT NULL DEFAULT '{}'::jsonb,
    protocol_version  integer NOT NULL DEFAULT 0,
    binary_version    text NOT NULL DEFAULT '',
    connected_at      timestamptz,
    last_heartbeat_at timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- A job is one agent run for one phase of one task.
CREATE TABLE jobs (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id          uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    project_id       uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    phase            text NOT NULL,
    role             text NOT NULL,
    backend          text NOT NULL,
    instruction      text NOT NULL DEFAULT '',
    status           text NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued', 'leased', 'running', 'stalled', 'done', 'failed', 'stopped')),
    runner_id        uuid REFERENCES runners(id) ON DELETE SET NULL,
    attempts         integer NOT NULL DEFAULT 0,
    max_attempts     integer NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    lease_expires_at timestamptz,
    last_event_at    timestamptz,
    bounds           jsonb NOT NULL DEFAULT '{}'::jsonb,
    receipt          jsonb,
    stop_reason      text,
    created_by_kind  text NOT NULL DEFAULT 'system',
    created_by       uuid,
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz,
    finished_at      timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_queue_idx ON jobs (created_at) WHERE status = 'queued';
CREATE INDEX jobs_task_idx ON jobs (task_id, created_at);
CREATE INDEX jobs_runner_active_idx ON jobs (runner_id) WHERE status IN ('leased', 'running', 'stalled');
CREATE INDEX jobs_lease_idx ON jobs (lease_expires_at) WHERE status IN ('leased', 'running', 'stalled');

-- Streamed agent output. (job_id, seq) makes a resend after reconnect a no-op.
CREATE TABLE job_events (
    job_id     uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    seq        bigint NOT NULL,
    type       text NOT NULL,
    payload    jsonb NOT NULL DEFAULT '{}'::jsonb,
    at         timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, seq)
);

-- +goose Down
DROP TABLE IF EXISTS job_events;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS runners;
