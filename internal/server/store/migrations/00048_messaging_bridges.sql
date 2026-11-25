-- +goose Up
-- Bridges to the places teams already talk (Slack, GitHub comments): a plugin with the messaging
-- capability carries what is said there into a project's conversations and back. Everything here is
-- a project's own: the same Slack workspace connected to two projects is two sets of identities and
-- links, and nothing one project's bridge learned is used for another.

-- Who said something outside, as the plugin that vouches for it (provider = its name, set by the
-- core, never by the plugin) knows them: a Slack user is T0123/U0456, a GitHub login is its id. A
-- person links their account with a code they made in Gator (external_link_codes); until then they
-- are 'external', whom an agent hears but who approves nothing and allows no agent to work.
CREATE TABLE external_identities (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    provider     text NOT NULL CHECK (provider ~ '^[a-z][a-z0-9-]{0,62}$'),
    external_id  text NOT NULL CHECK (length(external_id) BETWEEN 1 AND 200),
    display_name text NOT NULL DEFAULT '',
    user_id      uuid REFERENCES users(id) ON DELETE SET NULL,
    linked_at    timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, provider, external_id),
    CHECK ((user_id IS NULL) = (linked_at IS NULL))
);
CREATE INDEX external_identities_user_idx ON external_identities (user_id) WHERE user_id IS NOT NULL;

-- Where a conversation lives outside: a Slack channel or thread, a pull request's comments. One
-- outside home per provider for a conversation, and one conversation per outside place.
CREATE TABLE external_conversation_refs (
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    provider        text NOT NULL,
    external_ref    text NOT NULL CHECK (length(external_ref) BETWEEN 1 AND 400),
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, provider, external_ref),
    UNIQUE (provider, conversation_id)
);
-- Which bridges a conversation (or the one its root lives in) has, whatever the provider.
CREATE INDEX external_conversation_refs_conversation_idx ON external_conversation_refs (conversation_id);

-- A message's counterpart outside: the Slack message (team/channel/ts) a message came from or was
-- mirrored to. A redelivered event finds its message here and writes nothing new.
CREATE TABLE external_message_refs (
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    provider     text NOT NULL,
    external_ref text NOT NULL CHECK (length(external_ref) BETWEEN 1 AND 400),
    message_id   uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, provider, external_ref),
    UNIQUE (provider, message_id)
);

-- A code a person makes in Gator and types where the bridge hears it, to say "that account is
-- me": only its hash is kept, it works once, for one project and provider, for ten minutes.
CREATE TABLE external_link_codes (
    hash       bytea PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    provider   text NOT NULL,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX external_link_codes_user_idx ON external_link_codes (user_id);

-- A turn asked for from outside: by someone on a bridge, or by an A2A client.
ALTER TABLE agent_turns DROP CONSTRAINT agent_turns_requested_by_kind_check;
ALTER TABLE agent_turns ADD CONSTRAINT agent_turns_requested_by_kind_check
    CHECK (requested_by_kind IN ('user', 'agent', 'automation', 'process', 'external'));
-- What somebody outside asked for, and what agents delegated from it: an outsider's daily budget
-- reads both.
CREATE INDEX agent_turns_external_idx ON agent_turns (requested_by, created_at) WHERE requested_by_kind = 'external';
CREATE INDEX agent_turns_parent_idx ON agent_turns (parent_turn_id) WHERE parent_turn_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS agent_turns_parent_idx;
DROP INDEX IF EXISTS agent_turns_external_idx;
-- Turns asked for from outside stay, as asked by the process: their record outlives the bridge.
UPDATE agent_turns SET requested_by_kind = 'process' WHERE requested_by_kind = 'external';
ALTER TABLE agent_turns DROP CONSTRAINT agent_turns_requested_by_kind_check;
ALTER TABLE agent_turns ADD CONSTRAINT agent_turns_requested_by_kind_check
    CHECK (requested_by_kind IN ('user', 'agent', 'automation', 'process'));
DROP TABLE IF EXISTS external_link_codes;
DROP TABLE IF EXISTS external_message_refs;
DROP TABLE IF EXISTS external_conversation_refs;
DROP TABLE IF EXISTS external_identities;
