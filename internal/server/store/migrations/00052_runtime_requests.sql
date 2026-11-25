-- +goose Up
-- An agent waiting for a person mid-turn: a tool the project's approvals policy asks about, or a
-- question. The id is the runner's (a request sent again is the same request); the input is
-- redacted like logs before it is kept. A request is decided once, by a member of the project who
-- may write where the agent works, or expires and is refused; a turn that ends cancels it.

CREATE TABLE runtime_requests (
    id          uuid PRIMARY KEY,
    job_id      uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    turn_id     uuid REFERENCES agent_turns(id) ON DELETE SET NULL,
    kind        text NOT NULL CHECK (kind IN ('tool_approval', 'question')),
    tool        text,
    input       jsonb NOT NULL DEFAULT '{}',
    rule        text NOT NULL DEFAULT '',                 -- the rule of the policy that asks
    tool_use_id text NOT NULL DEFAULT '',
    status      text NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'allowed', 'allowed_for_session', 'denied', 'expired', 'cancelled')),
    stopped     boolean NOT NULL DEFAULT false,           -- denied by stopping the turn
    feedback    text,                                     -- why it was denied, or a question's answer
    decided_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    decided_at  timestamptz,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'pending') = (decided_at IS NULL))
);
CREATE INDEX runtime_requests_job_idx ON runtime_requests (job_id, created_at);
CREATE INDEX runtime_requests_pending_idx ON runtime_requests (expires_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE IF EXISTS runtime_requests;
