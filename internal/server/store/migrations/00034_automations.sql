-- +goose Up
-- A project's automations: the fixed catalogue Gator ships (PR review, a weekly dependency scan
-- and so on), each switched on or off per project. Switching one on is a person allowing agents
-- to work without asking on every task it creates, so the row keeps who did it and when: those
-- tasks carry that person as the one who allowed agents on them.
CREATE TABLE project_automations (
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key         text NOT NULL,
    enabled     boolean NOT NULL DEFAULT false,
    settings    jsonb NOT NULL DEFAULT '{}'::jsonb,
    enabled_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    enabled_at  timestamptz,
    last_run_at timestamptz,
    -- Why the last run did not do what it should, in words a person reads; empty when it did.
    last_error  text NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, key)
);

-- One row per thing an automation acted on: a pull request's commit, an incident, a week. The
-- unique key is what keeps a redelivered webhook or a restarted server from doing it twice.
CREATE TABLE automation_runs (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    automation text NOT NULL,
    subject    text NOT NULL,
    task_id    uuid REFERENCES tasks(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, automation, subject)
);

-- Which automation created a task, so a person reading it knows why it exists.
ALTER TABLE tasks ADD COLUMN automation text;

-- +goose Down
ALTER TABLE tasks DROP COLUMN automation;
DROP TABLE automation_runs;
DROP TABLE project_automations;
