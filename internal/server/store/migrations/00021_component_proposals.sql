-- +goose Up
-- The catalogue reaches every prompt, so it must not grow by itself. A component the server
-- worked out from where jobs actually touch code waits as a proposal until a person says yes.
ALTER TABLE components
    ADD COLUMN status text NOT NULL DEFAULT 'approved' CHECK (status IN ('approved', 'proposed')),
    ADD COLUMN proposed_reason text NOT NULL DEFAULT '';

-- Evidence, gathered across jobs. One job touching a directory once proves nothing; the same
-- directory being worked in again and again is what a component actually is.
CREATE TABLE component_hints (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    path         text NOT NULL,
    jobs         integer NOT NULL DEFAULT 0,
    files        integer NOT NULL DEFAULT 0,
    last_job_id  uuid REFERENCES jobs(id) ON DELETE SET NULL,
    proposed_at  timestamptz,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, path)
);

-- +goose Down
DROP TABLE component_hints;
ALTER TABLE components DROP COLUMN proposed_reason, DROP COLUMN status;
