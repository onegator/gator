-- +goose Up
-- Remote MCP servers agents reach through Gator's proxy (/mcp/proxy/<name>): their credentials stay
-- here, encrypted with the server's keyring like project_plugins.secrets, and a runner only ever
-- holds a job's own token. Every call through the proxy is written down.

CREATE TABLE mcp_servers (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       uuid REFERENCES projects(id) ON DELETE CASCADE,   -- NULL: every project's
    name             text NOT NULL CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$' AND name <> 'gator'),
    display_name     text NOT NULL DEFAULT '',
    url              text NOT NULL,
    auth             text NOT NULL CHECK (auth IN ('oauth', 'bearer', 'none')),
    secrets          text NOT NULL DEFAULT '',          -- encrypted with the server's keyring
    -- The tools agents may call; the rest stay off until an admin switches them on.
    allowed_tools    text[] NOT NULL DEFAULT '{}',
    -- Of those, the ones chat agents may call: read-only, as the admin (or the package a person
    -- accepted) said when allowing them. What a server says of its tools live is not trusted.
    read_only_tools  text[] NOT NULL DEFAULT '{}',
    enabled          boolean NOT NULL DEFAULT true,
    -- How a person connects it with OAuth: {provider, authorize_url, token_url, scopes}.
    oauth            jsonb NOT NULL DEFAULT '{}',
    token_expires_at timestamptz,
    source           jsonb NOT NULL DEFAULT '{}',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (project_id, name)
);

-- One line per call through the proxy, as plugin_calls are for plugins: who (job, agent), what
-- (method, tool, the arguments with secrets masked), how it went and how long it took.
CREATE TABLE mcp_calls (
    id            bigserial PRIMARY KEY,
    mcp_server_id uuid NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    project_id    uuid REFERENCES projects(id) ON DELETE CASCADE,
    job_id        uuid REFERENCES jobs(id) ON DELETE SET NULL,
    agent_id      uuid REFERENCES agents(id) ON DELETE SET NULL,
    method        text NOT NULL,
    tool          text NOT NULL DEFAULT '',
    payload       jsonb,
    outcome       text NOT NULL CHECK (outcome IN ('ok', 'error', 'denied', 'open')),
    error         text,
    result_bytes  integer NOT NULL DEFAULT 0,
    duration_ms   integer NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mcp_calls_server_idx ON mcp_calls (mcp_server_id, id DESC);
CREATE INDEX mcp_calls_job_idx ON mcp_calls (job_id) WHERE job_id IS NOT NULL;

-- Append-only, except for what a deletion elsewhere carries down (a server, a project, a job going):
-- those run inside the foreign key's own trigger.
-- +goose StatementBegin
CREATE FUNCTION mcp_calls_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF pg_trigger_depth() > 1 THEN
        RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    END IF;
    RAISE EXCEPTION 'table % is append-only', TG_TABLE_NAME;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER mcp_calls_append_only BEFORE UPDATE OR DELETE ON mcp_calls
    FOR EACH ROW EXECUTE FUNCTION mcp_calls_append_only();

-- Connecting a remote server with OAuth goes through the same /connect/callback as a plugin's: the
-- state says which of the two it is for, and plugin_name names the plugin or the server.
ALTER TABLE plugin_connect_states ADD COLUMN target text NOT NULL DEFAULT 'plugin' CHECK (target IN ('plugin', 'mcp'));
-- A connection starts at Gator (/connect/start), which ties it to the browser that opened it before
-- sending it on to this address; the callback takes only a browser that started it.
ALTER TABLE plugin_connect_states ADD COLUMN authorize_url text NOT NULL DEFAULT '';

-- +goose Down
DELETE FROM plugin_connect_states WHERE target <> 'plugin';
ALTER TABLE plugin_connect_states DROP COLUMN authorize_url;
ALTER TABLE plugin_connect_states DROP COLUMN target;
DROP TRIGGER mcp_calls_append_only ON mcp_calls;
DROP FUNCTION mcp_calls_append_only();
DROP TABLE mcp_calls;
DROP TABLE mcp_servers;
