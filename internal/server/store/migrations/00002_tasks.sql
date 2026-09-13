-- +goose Up
CREATE TABLE tasks (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id           uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind                 text NOT NULL,
    title                text NOT NULL,
    phase                text NOT NULL,
    urgency              smallint NOT NULL DEFAULT 3 CHECK (urgency BETWEEN 1 AND 4),
    owner_kind           text CHECK (owner_kind IN ('user', 'runner')),
    owner_id             uuid,
    requirements_changed boolean NOT NULL DEFAULT false,
    blocked_reason       text,
    source_task_id       uuid REFERENCES tasks(id),
    external_refs        jsonb NOT NULL DEFAULT '{}'::jsonb,
    phase_entered_at     timestamptz NOT NULL DEFAULT now(),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    closed_at            timestamptz
);
CREATE INDEX tasks_project_phase_idx ON tasks (project_id, phase) WHERE closed_at IS NULL;

CREATE TABLE phase_transitions (
    id         bigserial PRIMARY KEY,
    task_id    uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    from_phase text,
    to_phase   text NOT NULL,
    kind       text NOT NULL CHECK (kind IN ('create', 'advance', 'rollback', 'handoff', 'auto_block', 'unblock', 'close')),
    actor_kind text NOT NULL CHECK (actor_kind IN ('user', 'runner', 'plugin', 'system')),
    actor_id   uuid,
    reason     text,
    evidence   jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX phase_transitions_task_idx ON phase_transitions (task_id, id);
CREATE TRIGGER phase_transitions_append_only BEFORE UPDATE OR DELETE ON phase_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_change();

CREATE TABLE artifacts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id     uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    phase       text NOT NULL,
    type        text NOT NULL,
    version     integer NOT NULL DEFAULT 1,
    content     text,
    url         text,
    approved_at timestamptz,
    approved_by uuid REFERENCES users(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_id, phase, type, version)
);

CREATE TABLE gates (
    task_id        uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    phase          text NOT NULL,
    checks         jsonb NOT NULL DEFAULT '[]'::jsonb,
    human_approved_by uuid REFERENCES users(id),
    human_approved_at timestamptz,
    blocked_reason text,
    blocked_by     text CHECK (blocked_by IN ('automation', 'human')),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, phase)
);

-- +goose Down
DROP TABLE IF EXISTS gates;
DROP TABLE IF EXISTS artifacts;
DROP TRIGGER IF EXISTS phase_transitions_append_only ON phase_transitions;
DROP TABLE IF EXISTS phase_transitions;
DROP TABLE IF EXISTS tasks;
