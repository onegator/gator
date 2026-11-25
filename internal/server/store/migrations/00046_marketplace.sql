-- +goose Up
-- The marketplace: registries (git repositories, or directories, with a signed index.json), the
-- packages they offer as their last fetch found them, what a project or the workspace installed
-- from them and with which permissions accepted, and the skills agents carry.

CREATE TABLE marketplace_registries (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    url         text NOT NULL CHECK (url <> ''),
    -- The keys its index is signed with ("ed25519:<base64>", comma-separated); empty: an unsigned
    -- registry whose packages only their checksums vouch for.
    public_key  text NOT NULL DEFAULT '',
    builtin     boolean NOT NULL DEFAULT false,
    fetched_at  timestamptz,
    -- Why the last fetch failed, or which packages it turned away; empty when all was well.
    fetch_error text NOT NULL DEFAULT '',
    created_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
-- Gator's own registry is one.
CREATE UNIQUE INDEX marketplace_registries_builtin_idx ON marketplace_registries (builtin) WHERE builtin;

-- What a registry offers, every version any fetch verified. A published version never changes:
-- a later fetch whose files hash differently is refused.
CREATE TABLE marketplace_items (
    registry_id uuid NOT NULL REFERENCES marketplace_registries(id) ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('agent', 'skill', 'integration', 'process', 'knowledge')),
    name        text NOT NULL,
    version     text NOT NULL,
    manifest    jsonb NOT NULL,          -- marketplace.Manifest: what the catalogue shows and what installing asks
    checksum    text NOT NULL,
    files       jsonb NOT NULL DEFAULT '[]',
    -- Whether the registry's last fetch still listed it: a version it dropped stays installable
    -- where it is installed, but is not offered.
    listed      boolean NOT NULL DEFAULT true,
    fetched_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (registry_id, kind, name, version)
);
CREATE INDEX marketplace_items_name_idx ON marketplace_items (kind, name);

CREATE TABLE installations (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id           uuid REFERENCES projects(id) ON DELETE CASCADE,   -- NULL: the workspace
    registry_id          uuid NOT NULL REFERENCES marketplace_registries(id),
    kind                 text NOT NULL CHECK (kind IN ('agent', 'skill', 'integration', 'process', 'knowledge')),
    name                 text NOT NULL,
    version              text NOT NULL,
    checksum             text NOT NULL,
    target_id            uuid,           -- agents.id / skills.id / mcp_servers.id / plugins.id / knowledge_packs.id
    accepted_permissions jsonb NOT NULL, -- exactly what a person accepted
    -- What it installed, to tell an update from a local edit: the manifest and, per kind, the
    -- text a person may change afterwards (an agent's instructions).
    installed            jsonb NOT NULL DEFAULT '{}',
    installed_by         uuid NOT NULL REFERENCES users(id),
    installed_at         timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (project_id, kind, name)
);
CREATE INDEX installations_target_idx ON installations (target_id) WHERE target_id IS NOT NULL;

-- A skill is a folder in the Agent Skills format (SKILL.md, scripts/, references/, assets/),
-- kept whole: a runner gets it in the lease of a job whose agent carries it.
CREATE TABLE skills (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid REFERENCES projects(id) ON DELETE CASCADE,   -- NULL: every project's
    name        text NOT NULL CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    version     text NOT NULL,
    description text NOT NULL,
    files       jsonb NOT NULL,
    source      jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (project_id, name)
);

-- +goose Down
DROP TABLE skills;
DROP TABLE installations;
DROP TABLE marketplace_items;
DROP TABLE marketplace_registries;
