-- +goose Up
-- Installed plugins: how to start the process and the manifest it reported.
CREATE TABLE plugins (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL UNIQUE,
    command    text[] NOT NULL,
    version    text NOT NULL DEFAULT '',
    manifest   jsonb NOT NULL DEFAULT '{}'::jsonb,
    enabled    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- A plugin enabled for one project. Secrets are encrypted with the server keyring and reach
-- only this project's process, as environment variables.
CREATE TABLE project_plugins (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    plugin_id       uuid NOT NULL REFERENCES plugins(id) ON DELETE CASCADE,
    config          jsonb NOT NULL DEFAULT '{}'::jsonb,
    secrets         text NOT NULL DEFAULT '',
    enabled         boolean NOT NULL DEFAULT true,
    disabled_reason text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, plugin_id)
);

-- Every call in either direction, with secrets redacted. Append-only.
CREATE TABLE plugin_calls (
    id                bigserial PRIMARY KEY,
    project_plugin_id uuid NOT NULL REFERENCES project_plugins(id) ON DELETE CASCADE,
    direction         text NOT NULL CHECK (direction IN ('core_to_plugin', 'plugin_to_core')),
    method            text NOT NULL,
    payload           jsonb,
    result            jsonb,
    error             text,
    duration_ms       integer NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX plugin_calls_pp_idx ON plugin_calls (project_plugin_id, id DESC);
CREATE TRIGGER plugin_calls_append_only BEFORE UPDATE OR DELETE ON plugin_calls
    FOR EACH ROW EXECUTE FUNCTION forbid_change();

-- A webhook delivery is handled once; a redelivery with the same id is a no-op.
CREATE TABLE webhook_deliveries (
    project_plugin_id uuid NOT NULL REFERENCES project_plugins(id) ON DELETE CASCADE,
    delivery_id       text NOT NULL,
    received_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_plugin_id, delivery_id)
);

-- A small key-value store per plugin and project.
CREATE TABLE plugin_kv (
    project_plugin_id uuid NOT NULL REFERENCES project_plugins(id) ON DELETE CASCADE,
    key               text NOT NULL,
    value             jsonb NOT NULL,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_plugin_id, key)
);

-- How far the plugin host has delivered domain events. Plugins read the outbox with their
-- own cursor, so a hook survives a restart or a dropped hub subscription.
CREATE TABLE plugin_event_cursor (
    id            integer PRIMARY KEY CHECK (id = 1),
    last_event_id bigint NOT NULL
);

-- +goose Down
DROP TABLE plugin_event_cursor;
DROP TABLE plugin_kv;
DROP TABLE webhook_deliveries;
DROP TRIGGER IF EXISTS plugin_calls_append_only ON plugin_calls;
DROP TABLE plugin_calls;
DROP TABLE project_plugins;
DROP TABLE plugins;
