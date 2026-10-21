-- +goose Up
-- A native client (Gator.app, the CLI) signs in through the browser, but the browser cannot
-- hand it a session cookie. The OIDC callback leaves a one-time code here instead, bound to
-- the PKCE challenge the client sent, and the client trades the code for its own token. Only
-- the code's hash is stored, and a code lives for minutes.
CREATE TABLE login_codes (
    hash           bytea PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_challenge text NOT NULL,
    redirect_uri   text NOT NULL,
    expires_at     timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE login_codes;
