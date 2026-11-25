-- +goose Up
-- Agents outside Gator, through A2A (Agent2Agent). Outbound: an agent of kind 'remote' is a project's
-- member like any other (a handle, a profile, the same limits), whose turns are A2A tasks sent to
-- the endpoint its Agent Card names, with a credential kept here, encrypted. Inbound: a project's
-- admin mints an A2A client a token (gtr_a2a_…) and a daily budget; the client asks the agents the
-- project exposes, in #a2a-<client>, where people see who asked what.

ALTER TABLE agents
    ADD COLUMN kind text NOT NULL DEFAULT 'local' CHECK (kind IN ('local', 'remote')),
    -- {card_url, url, name, description, version, skills: [{id, name, description}], fetched_at}
    ADD COLUMN remote jsonb NOT NULL DEFAULT '{}',
    ADD COLUMN remote_secrets text NOT NULL DEFAULT '',    -- encrypted with the server's keyring
    ADD COLUMN a2a_exposed boolean NOT NULL DEFAULT false;
-- A remote agent is a project's, chats only and plays no role: its answers are outside words.
ALTER TABLE agents ADD CONSTRAINT agents_remote_check
    CHECK (kind = 'local' OR (project_id IS NOT NULL AND tier = 'chat' AND role_key IS NULL AND NOT a2a_exposed));

ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_kind_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_kind_check CHECK (kind IN ('user', 'runner', 'plugin', 'job', 'a2a'));

CREATE TABLE a2a_clients (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name             text NOT NULL CHECK (name ~ '^[a-z][a-z0-9-]{0,39}$'),
    token_id         uuid NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    -- Who the client is in conversations: an outside identity (provider 'a2a') of the project.
    identity_id      uuid NOT NULL REFERENCES external_identities(id) ON DELETE CASCADE,
    daily_budget_usd double precision NOT NULL DEFAULT 5 CHECK (daily_budget_usd >= 0),
    per_minute       integer NOT NULL DEFAULT 30 CHECK (per_minute BETWEEN 1 AND 600),
    created_by       uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    revoked_at       timestamptz,
    UNIQUE (project_id, name)
);

-- What a client asked: the A2A task id it was given is this row's id, the message it became, and
-- the agent asked. The turn is found by (agent, message).
CREATE TABLE a2a_tasks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id   uuid NOT NULL REFERENCES a2a_clients(id) ON DELETE CASCADE,
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    agent_id    uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    message_id  uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    context_id  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (message_id, agent_id)
);

-- One call to a remote agent per turn: claimed by one worker at a time, retried when a claim
-- expires, ended once with what the remote said.
CREATE TABLE a2a_calls (
    turn_id        uuid PRIMARY KEY REFERENCES agent_turns(id) ON DELETE CASCADE,
    agent_id       uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    status         text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'calling', 'done', 'failed')),
    remote_task_id text NOT NULL DEFAULT '',
    attempts       integer NOT NULL DEFAULT 0,
    error          text NOT NULL DEFAULT '',
    claimed_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz
);
CREATE INDEX a2a_calls_open_idx ON a2a_calls (created_at) WHERE status IN ('pending', 'calling');

-- +goose Down
DROP TABLE IF EXISTS a2a_calls;
DROP TABLE IF EXISTS a2a_tasks;
DROP TABLE IF EXISTS a2a_clients;
DELETE FROM api_tokens WHERE kind = 'a2a';
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_kind_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_kind_check CHECK (kind IN ('user', 'runner', 'plugin', 'job'));
-- Remote agents go with their kind; their turns go with them (agent_turns holds them RESTRICT).
DELETE FROM agent_turns WHERE agent_id IN (SELECT id FROM agents WHERE kind = 'remote');
DELETE FROM agents WHERE kind = 'remote';
ALTER TABLE agents DROP CONSTRAINT agents_remote_check;
ALTER TABLE agents DROP COLUMN a2a_exposed, DROP COLUMN remote_secrets, DROP COLUMN remote, DROP COLUMN kind;
