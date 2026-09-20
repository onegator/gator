-- +goose Up
-- What reached production, and what production said about it afterwards. Until now a task
-- ended at approved: the part where software meets its users was outside the system.
CREATE TABLE releases (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id        uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id           uuid REFERENCES tasks(id) ON DELETE SET NULL,
    version           text NOT NULL,
    commit_sha        text NOT NULL DEFAULT '',
    url               text NOT NULL DEFAULT '',
    environment       text NOT NULL DEFAULT 'production',
    source_plugin_id  uuid REFERENCES project_plugins(id) ON DELETE SET NULL,
    deployed_at       timestamptz NOT NULL DEFAULT now(),
    -- How long to watch before calling it good. The window is the whole point of the
    -- Monitoring phase: a deploy is not finished the moment it lands.
    observation_until timestamptz,
    settled_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, version, environment)
);

CREATE TABLE incidents (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    source_plugin_id uuid REFERENCES project_plugins(id) ON DELETE SET NULL,
    external_id      text NOT NULL DEFAULT '',
    -- Monitoring tools repeat themselves: the same fault arrives again every few seconds.
    -- The fingerprint is how one fault stays one incident.
    fingerprint      text NOT NULL,
    title            text NOT NULL,
    severity         text NOT NULL DEFAULT 'medium' CHECK (severity IN ('critical', 'high', 'medium', 'low')),
    url              text NOT NULL DEFAULT '',
    count            integer NOT NULL DEFAULT 1,
    release_id       uuid REFERENCES releases(id) ON DELETE SET NULL,
    task_id          uuid REFERENCES tasks(id) ON DELETE SET NULL,
    first_seen_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at     timestamptz NOT NULL DEFAULT now(),
    closed_at        timestamptz,
    UNIQUE (project_id, fingerprint)
);

CREATE INDEX releases_watching_idx ON releases (observation_until) WHERE settled_at IS NULL AND observation_until IS NOT NULL;
CREATE INDEX incidents_open_idx ON incidents (project_id, last_seen_at) WHERE closed_at IS NULL;

-- +goose Down
DROP INDEX incidents_open_idx;
DROP INDEX releases_watching_idx;
DROP TABLE incidents;
DROP TABLE releases;
