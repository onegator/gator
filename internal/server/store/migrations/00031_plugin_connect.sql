-- +goose Up
-- A tracker can say an issue was reopened, and the task it became opens again with it. The
-- 'reopen' transition kind that records it comes from 00030, with the people's reopen.

-- "Connect GitHub" sends a person's browser to the provider and back. The state names the
-- project and plugin the answer is for and carries the PKCE verifier; it is single-use and
-- lives for minutes. Only its hash is stored, like a login code.
CREATE TABLE plugin_connect_states (
    hash        bytea PRIMARY KEY,
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    plugin_name text NOT NULL,
    user_id     uuid REFERENCES users(id) ON DELETE CASCADE,
    verifier    text NOT NULL,
    return_to   text NOT NULL DEFAULT '',
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- A connected plugin's token may expire; the server refreshes it before it does. NULL: it does
-- not expire, or it was pasted by a person.
ALTER TABLE project_plugins ADD COLUMN token_expires_at timestamptz;

-- +goose Down
ALTER TABLE project_plugins DROP COLUMN token_expires_at;
DROP TABLE plugin_connect_states;
