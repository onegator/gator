-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email       text NOT NULL UNIQUE,
    name        text NOT NULL,
    oidc_subject text UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE projects (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug           text NOT NULL UNIQUE,
    name           text NOT NULL,
    tags           text[] NOT NULL DEFAULT '{}',
    process_config jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE project_repos (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name           text NOT NULL,
    url            text NOT NULL,
    default_branch text NOT NULL DEFAULT 'main',
    is_primary     boolean NOT NULL DEFAULT false,
    UNIQUE (project_id, name)
);

CREATE TABLE memberships (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       text NOT NULL CHECK (role IN ('admin', 'member', 'viewer')),
    PRIMARY KEY (project_id, user_id)
);

CREATE TABLE api_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       text NOT NULL CHECK (kind IN ('user', 'runner', 'plugin')),
    scope      text NOT NULL,
    hash       bytea NOT NULL UNIQUE,
    user_id    uuid REFERENCES users(id) ON DELETE CASCADE,
    expires_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Domain outbox: every operation inserts here in its own transaction; the relay publishes.
CREATE TABLE events (
    id           bigserial PRIMARY KEY,
    type         text NOT NULL,
    aggregate    text NOT NULL,
    aggregate_id uuid,
    payload      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX events_unpublished_idx ON events (id) WHERE published_at IS NULL;

-- Proof that something happened, from a runner, a plugin, or the API.
CREATE TABLE receipts (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source       text NOT NULL CHECK (source IN ('runner', 'plugin', 'api')),
    subject_kind text NOT NULL,
    subject_id   uuid NOT NULL,
    status       text NOT NULL,
    payload      jsonb NOT NULL DEFAULT '{}'::jsonb,
    verified_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX receipts_subject_idx ON receipts (subject_kind, subject_id);

CREATE TABLE audit_log (
    id         bigserial PRIMARY KEY,
    actor_kind text NOT NULL,
    actor_id   uuid,
    action     text NOT NULL,
    target     text NOT NULL,
    payload    jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Append-only guard for audit tables.
-- +goose StatementBegin
CREATE FUNCTION forbid_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'table % is append-only', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION forbid_change();
CREATE TRIGGER receipts_append_only BEFORE UPDATE OR DELETE ON receipts
    FOR EACH ROW EXECUTE FUNCTION forbid_change();

-- +goose Down
DROP TRIGGER IF EXISTS receipts_append_only ON receipts;
DROP TRIGGER IF EXISTS audit_log_append_only ON audit_log;
DROP FUNCTION IF EXISTS forbid_change();
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS receipts;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS project_repos;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS users;
