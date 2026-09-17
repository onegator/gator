-- +goose Up
-- What a project knows about its own product: the owner's vision and principles, and the
-- decisions and lessons the work produces. Never shared between projects.
CREATE TABLE product_context (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind            text NOT NULL CHECK (kind IN ('vision', 'principles', 'persona', 'glossary', 'decision', 'lesson', 'roadmap')),
    title           text NOT NULL,
    content         text NOT NULL,
    version         integer NOT NULL,
    -- an agent proposes; a person approves. Only approved entries reach a job's prompt.
    status          text NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed', 'approved', 'archived')),
    source_task_id  uuid REFERENCES tasks(id) ON DELETE SET NULL,
    created_by_kind text NOT NULL DEFAULT 'system',
    created_by      uuid,
    approved_by     uuid REFERENCES users(id),
    approved_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, kind, title, version)
);
CREATE INDEX product_context_project_idx ON product_context (project_id, kind, status);
CREATE INDEX product_context_task_idx ON product_context (source_task_id) WHERE source_task_id IS NOT NULL;

-- +goose Down
DROP TABLE product_context;
