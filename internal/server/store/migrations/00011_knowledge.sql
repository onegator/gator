-- +goose Up
-- Technical knowledge: how this workspace builds software. Shared across projects, unlike
-- product context, which never leaves its project.

-- A pack fetched from a registry, cached whole so a job never waits on the network.
CREATE TABLE knowledge_packs (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    version    text NOT NULL,
    source     text NOT NULL CHECK (source IN ('git', 'manual')),
    url        text NOT NULL DEFAULT '',
    checksum   text NOT NULL DEFAULT '',
    manifest   jsonb NOT NULL DEFAULT '{}'::jsonb,
    files      jsonb NOT NULL DEFAULT '[]'::jsonb,
    fetched_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (name, version)
);

-- An entry written here: workspace-wide when project_id is null, otherwise a project's own
-- rule or its override of a pack's.
CREATE TABLE knowledge_entries (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid REFERENCES projects(id) ON DELETE CASCADE,
    scope      text NOT NULL CHECK (scope IN ('language', 'architecture', 'requirements', 'testing', 'security')),
    title      text NOT NULL,
    content    text NOT NULL,
    position   integer NOT NULL DEFAULT 100,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX knowledge_entries_scope_idx ON knowledge_entries (project_id, scope, position);

-- Which packs a project switches on.
CREATE TABLE project_packs (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    pack_id    uuid NOT NULL REFERENCES knowledge_packs(id) ON DELETE CASCADE,
    position   integer NOT NULL DEFAULT 100,
    PRIMARY KEY (project_id, pack_id)
);

-- +goose Down
DROP TABLE project_packs;
DROP TABLE knowledge_entries;
DROP TABLE knowledge_packs;
