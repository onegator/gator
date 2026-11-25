-- +goose Up
-- A person's own model logins: a Claude subscription token, an Anthropic API key, a ChatGPT login
-- (codex's auth.json) or an OpenAI API key. A job runs on the login of the person who pays for it,
-- handed to the runner with the lease, rather than on whatever the runner's own environment holds.
-- The secret is encrypted with the server's keyring like mcp_servers.secrets and never leaves the
-- server except in a lease; the API shows its kind, a hint (its last four characters) and how it is.
CREATE TABLE user_credentials (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind         text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_]{0,39}$'),
    -- The backend the kind serves ("claude", "acp:codex"), as the kind said when it was saved, so a
    -- query can tell whether a person can pay for a job without knowing the kinds.
    backend      text NOT NULL,
    secret       text NOT NULL,                       -- encrypted with the server's keyring
    hint         text NOT NULL DEFAULT '',
    -- unchecked: well formed, never tried; ok: the last check or job went through; error: refused.
    status       text NOT NULL DEFAULT 'unchecked' CHECK (status IN ('unchecked', 'ok', 'error')),
    last_error   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    checked_at   timestamptz,
    last_used_at timestamptz,
    UNIQUE (user_id, kind)
);

-- Who pays for a job (decided when it is queued), which login it was leased with (decided when it
-- is leased: '' not yet, 'runner' the runner's own, else a kind), and why it waits when it cannot
-- be leased for want of one.
ALTER TABLE jobs
    -- A person, like created_by: no foreign key, so a job's record outlives its people.
    ADD COLUMN payer_id uuid,
    ADD COLUMN credential_id uuid REFERENCES user_credentials(id) ON DELETE SET NULL,
    ADD COLUMN credential_kind text NOT NULL DEFAULT '',
    -- A login whose refresh token rotates is held by one job at a time; see the index below.
    ADD COLUMN credential_exclusive boolean NOT NULL DEFAULT false,
    ADD COLUMN waiting_reason text NOT NULL DEFAULT '';

-- The lease takes a lock and checks before it hands such a login out; this is the backstop: two
-- active jobs can never hold the same exclusive login.
CREATE UNIQUE INDEX jobs_exclusive_credential_idx ON jobs (credential_id)
    WHERE credential_exclusive AND status IN ('leased', 'running', 'stalled');
CREATE INDEX jobs_payer_idx ON jobs (payer_id) WHERE payer_id IS NOT NULL;

-- Whose login paid for the usage: the payer when a job ran on theirs, nobody when it ran on the
-- runner's own ('runner').
ALTER TABLE usage_records
    ADD COLUMN paid_by uuid REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN credential_kind text NOT NULL DEFAULT '';
CREATE INDEX usage_records_paid_by_idx ON usage_records (paid_by, created_at) WHERE paid_by IS NOT NULL;

-- Whether a job whose payer has no login of their own may run on the runner's (the operator's
-- environment, GATOR_CLOUD_RUNNER_ENV). Off for a new workspace. A workspace that already ran jobs
-- ran every one of them that way, so it keeps doing so until an admin turns it off.
ALTER TABLE workspace_settings ADD COLUMN runner_logins boolean NOT NULL DEFAULT false;
UPDATE workspace_settings SET runner_logins = EXISTS (SELECT 1 FROM jobs);

-- +goose Down
ALTER TABLE workspace_settings DROP COLUMN runner_logins;
DROP INDEX usage_records_paid_by_idx;
ALTER TABLE usage_records DROP COLUMN credential_kind, DROP COLUMN paid_by;
DROP INDEX jobs_payer_idx;
DROP INDEX jobs_exclusive_credential_idx;
ALTER TABLE jobs DROP COLUMN waiting_reason, DROP COLUMN credential_exclusive, DROP COLUMN credential_kind,
    DROP COLUMN credential_id, DROP COLUMN payer_id;
DROP TABLE user_credentials;
