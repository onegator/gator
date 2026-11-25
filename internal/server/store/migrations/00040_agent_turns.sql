-- +goose Up
-- One turn is one answer of one agent to one call: a mention, a message in a DM, a delegation from
-- another agent, an escalation a person allowed. Refused calls are turns too, with the reason, so
-- that "why did @planner not answer" has an answer.
CREATE TABLE agent_turns (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id           uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    project_id         uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    conversation_id    uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,  -- where the answer goes
    trigger_message_id uuid REFERENCES messages(id) ON DELETE SET NULL,
    reply_message_id   uuid REFERENCES messages(id) ON DELETE SET NULL,
    requested_by_kind  text NOT NULL CHECK (requested_by_kind IN ('user', 'agent', 'automation', 'process')),
    requested_by       uuid,
    -- The person at the start of the chain (phase 3: whose private context the chain may read).
    on_behalf_of       uuid REFERENCES users(id) ON DELETE SET NULL,
    chain_id           uuid NOT NULL,                     -- shared by agent-to-agent delegations
    parent_turn_id     uuid REFERENCES agent_turns(id) ON DELETE SET NULL,   -- the turn that delegated
    escalated_from     uuid REFERENCES agent_turns(id) ON DELETE SET NULL,   -- the chat turn a person let work
    hop                smallint NOT NULL DEFAULT 0 CHECK (hop >= 0),
    tier               text NOT NULL DEFAULT 'chat' CHECK (tier IN ('chat', 'work')),
    job_id             uuid REFERENCES jobs(id) ON DELETE SET NULL,
    status             text NOT NULL DEFAULT 'queued'
                       CHECK (status IN ('queued', 'running', 'done', 'failed', 'refused', 'stopped')),
    refusal            text,                              -- 'hop_limit', 'chain_budget', 'budget', 'paused', …
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz,
    CHECK ((status = 'refused') = (refusal IS NOT NULL)),
    UNIQUE (agent_id, trigger_message_id)                 -- a redelivered mention makes no second answer
);
CREATE INDEX agent_turns_chain_idx ON agent_turns (chain_id, created_at);
CREATE INDEX agent_turns_active_idx ON agent_turns (conversation_id) WHERE status IN ('queued', 'running');
CREATE INDEX agent_turns_agent_idx ON agent_turns (agent_id, created_at DESC);

-- What a job is: a phase of the process ('phase'), an answer in a conversation ('reply'), or the
-- summary of an older part of one ('summary'). And what it may touch: 'work' is today's full
-- worktree, 'chat' a read-only checkout with no tools that write.
ALTER TABLE jobs
    ADD COLUMN turn_id uuid REFERENCES agent_turns(id) ON DELETE SET NULL,
    ADD COLUMN tier text NOT NULL DEFAULT 'work' CHECK (tier IN ('chat', 'work')),
    ADD COLUMN kind text NOT NULL DEFAULT 'phase' CHECK (kind IN ('phase', 'reply', 'summary'));
CREATE INDEX jobs_turn_idx ON jobs (turn_id) WHERE turn_id IS NOT NULL;

-- The background of a long conversation, written by a cheap model (policy.roles.summarizer) so an
-- agent's prompt carries it instead of every old message. upto_seq is the last message it covers;
-- a newer summary being written is pending_job_id, covering up to pending_upto_seq.
CREATE TABLE conversation_summaries (
    conversation_id  uuid PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    upto_seq         bigint NOT NULL DEFAULT 0,
    summary          text NOT NULL DEFAULT '',
    model            text NOT NULL DEFAULT '',
    pending_job_id   uuid REFERENCES jobs(id) ON DELETE SET NULL,
    pending_upto_seq bigint,
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE conversation_summaries;
DROP INDEX jobs_turn_idx;
ALTER TABLE jobs DROP COLUMN kind, DROP COLUMN tier, DROP COLUMN turn_id;
DROP TABLE agent_turns;
