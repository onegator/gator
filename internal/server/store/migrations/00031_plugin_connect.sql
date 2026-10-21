-- +goose Up
-- A tracker can say an issue was reopened, and the task it became opens again with it. The
-- audit trail has to be able to say so.
ALTER TABLE phase_transitions DROP CONSTRAINT phase_transitions_kind_check;
ALTER TABLE phase_transitions ADD CONSTRAINT phase_transitions_kind_check
    CHECK (kind IN ('create', 'advance', 'rollback', 'handoff', 'auto_block', 'unblock', 'close', 'reopen'));

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
DELETE FROM phase_transitions WHERE kind = 'reopen';
ALTER TABLE phase_transitions DROP CONSTRAINT phase_transitions_kind_check;
ALTER TABLE phase_transitions ADD CONSTRAINT phase_transitions_kind_check
    CHECK (kind IN ('create', 'advance', 'rollback', 'handoff', 'auto_block', 'unblock', 'close'));
