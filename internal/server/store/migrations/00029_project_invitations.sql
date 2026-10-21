-- +goose Up
-- A project admin invites a person by email before that person has an account. The link the
-- admin passes on carries a token; only its hash is kept here, like every other secret the
-- server hands out. An invitation is open until it is accepted, revoked or expires, and a
-- project holds at most one open invitation per address: inviting again replaces it.
CREATE TABLE project_invitations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    email       text NOT NULL,
    role        text NOT NULL CHECK (role IN ('admin', 'member', 'viewer')),
    token_hash  bytea NOT NULL UNIQUE,
    invited_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    accepted_by uuid REFERENCES users(id) ON DELETE SET NULL,
    revoked_at  timestamptz
);
CREATE UNIQUE INDEX project_invitations_open_idx ON project_invitations (project_id, lower(email))
    WHERE accepted_at IS NULL AND revoked_at IS NULL;
CREATE INDEX project_invitations_email_idx ON project_invitations (lower(email))
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

-- +goose Down
DROP TABLE project_invitations;
