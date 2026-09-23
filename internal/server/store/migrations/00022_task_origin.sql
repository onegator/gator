-- +goose Up
-- Where a task's words came from. A task can be raised by a webhook — a GitHub issue, a
-- monitoring alert — which means somebody outside this workspace wrote the text that reaches an
-- agent's prompt, while the agent works with the operator's credentials. Marking the origin is
-- what lets the rest of the system treat that text as data rather than as instructions.
ALTER TABLE tasks
    ADD COLUMN origin text NOT NULL DEFAULT 'internal' CHECK (origin IN ('internal', 'external')),
    -- Who raised it, in words a person recognises: the plugin's name, usually.
    ADD COLUMN origin_source text NOT NULL DEFAULT '',
    -- A person letting an outside task through. Until then no agent starts on it by itself.
    ADD COLUMN admitted_at timestamptz,
    ADD COLUMN admitted_by uuid REFERENCES users(id) ON DELETE SET NULL;

-- Existing rows stay internal. A task's origin cannot be worked out after the fact: external
-- refs are also written onto tasks people created, so guessing would mislabel them, and a wrong
-- "internal" is worse than an honest "we did not record it".

CREATE INDEX tasks_waiting_admission_idx ON tasks (project_id)
    WHERE origin = 'external' AND admitted_at IS NULL AND closed_at IS NULL;

-- +goose Down
DROP INDEX tasks_waiting_admission_idx;
ALTER TABLE tasks DROP COLUMN admitted_by, DROP COLUMN admitted_at, DROP COLUMN origin_source, DROP COLUMN origin;
