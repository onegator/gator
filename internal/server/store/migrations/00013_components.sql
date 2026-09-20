-- +goose Up
-- A light catalogue of what a project is made of. Agents waste a whole discovery pass working
-- out which repository holds what and who owns it; this says so in one place.
CREATE TABLE components (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key        text NOT NULL,
    name       text NOT NULL,
    kind       text NOT NULL DEFAULT 'app' CHECK (kind IN ('app', 'api', 'lib', 'infra')),
    repo       text NOT NULL DEFAULT '',
    path       text NOT NULL DEFAULT '',
    owner_id   uuid REFERENCES users(id) ON DELETE SET NULL,
    notes      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, key)
);

-- What a component needs to work. Both sides belong to the same project; nothing here ever
-- points at another project's component.
CREATE TABLE component_deps (
    component_id  uuid NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    depends_on_id uuid NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    PRIMARY KEY (component_id, depends_on_id),
    CHECK (component_id <> depends_on_id)
);

-- The decisions that shaped a component. ADRs by another name: the project already records
-- decisions as product context, so they are linked rather than copied into a second place.
CREATE TABLE component_decisions (
    component_id uuid NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    entry_id     uuid NOT NULL REFERENCES product_context(id) ON DELETE CASCADE,
    PRIMARY KEY (component_id, entry_id)
);

-- Which part of the product a task is about, so a job can be told about that part alone.
ALTER TABLE tasks ADD COLUMN component_id uuid REFERENCES components(id) ON DELETE SET NULL;

-- How much context a job was handed, to compare a job that got the catalogue with one that
-- did not. Without this the saving is a claim rather than a measurement.
ALTER TABLE jobs
    ADD COLUMN context_bytes integer NOT NULL DEFAULT 0,
    ADD COLUMN context_docs  integer NOT NULL DEFAULT 0;

CREATE INDEX components_project_idx ON components (project_id, key);
CREATE INDEX tasks_component_idx ON tasks (component_id) WHERE component_id IS NOT NULL;

-- +goose Down
DROP INDEX tasks_component_idx;
DROP INDEX components_project_idx;
ALTER TABLE jobs DROP COLUMN context_docs, DROP COLUMN context_bytes;
ALTER TABLE tasks DROP COLUMN component_id;
DROP TABLE component_decisions;
DROP TABLE component_deps;
DROP TABLE components;
