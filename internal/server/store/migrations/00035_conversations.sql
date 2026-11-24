-- +goose Up
-- People in a project talk in conversations: named channels, direct messages, threads under one
-- message, and the one thread every task has. Members are polymorphic so that agents (phase 2)
-- join conversations the way people do; the data model expects them from the start.
CREATE TABLE conversations (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- NULL only for a direct message between people outside any project, and its threads.
    project_id        uuid REFERENCES projects(id) ON DELETE CASCADE,
    kind              text NOT NULL CHECK (kind IN ('channel', 'dm', 'thread', 'task')),
    -- Channels only; unique in the project (conversations_channel_name_idx).
    name              text,
    topic             text NOT NULL DEFAULT '',
    visibility        text NOT NULL DEFAULT 'project' CHECK (visibility IN ('project', 'private')),
    -- A thread's (or a task thread's) root message. Who may read a thread is decided by the
    -- conversation that root message lives in.
    parent_message_id uuid,
    task_id           uuid UNIQUE REFERENCES tasks(id) ON DELETE CASCADE,
    component_key     text,
    -- A direct message is identified by who is in it: the sorted member ids. Opening a DM with
    -- the same people in the same scope answers the one that exists.
    dm_key            text,
    archived_at       timestamptz,
    created_by_kind   text NOT NULL CHECK (created_by_kind IN ('user', 'agent', 'system', 'plugin', 'external')),
    created_by        uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    last_message_at   timestamptz,
    -- The seq of the newest message. Posting takes the next one with
    -- UPDATE ... SET last_seq = last_seq + 1 RETURNING last_seq, which locks the row for the rest
    -- of the transaction: messages are numbered without gaps, in commit order.
    last_seq          bigint NOT NULL DEFAULT 0,
    CHECK ((kind = 'channel') = (name IS NOT NULL)),
    CHECK ((kind = 'dm') = (dm_key IS NOT NULL)),
    CHECK ((kind = 'task') = (task_id IS NOT NULL)),
    CHECK (kind <> 'thread' OR parent_message_id IS NOT NULL),
    CHECK (kind IN ('dm', 'thread') OR project_id IS NOT NULL)
);
-- Channel names are unique per project. Channels always have a project, and other kinds have no
-- name, so this is a partial index rather than UNIQUE (project_id, name), which would make every
-- unnamed conversation of a project collide under NULLS NOT DISTINCT.
CREATE UNIQUE INDEX conversations_channel_name_idx ON conversations (project_id, name) WHERE kind = 'channel';
-- One DM per set of people per scope; a NULL project is one scope (outside every project).
CREATE UNIQUE INDEX conversations_dm_idx ON conversations (project_id, dm_key) NULLS NOT DISTINCT WHERE kind = 'dm';
-- One thread per message.
CREATE UNIQUE INDEX conversations_parent_idx ON conversations (parent_message_id) WHERE parent_message_id IS NOT NULL;
CREATE INDEX conversations_project_idx ON conversations (project_id, kind);

CREATE TABLE conversation_members (
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    member_kind     text NOT NULL CHECK (member_kind IN ('user', 'agent')),
    member_id       uuid NOT NULL,
    role            text NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'member')),
    notify          text NOT NULL DEFAULT 'mentions' CHECK (notify IN ('all', 'mentions', 'none')),
    last_read_seq   bigint NOT NULL DEFAULT 0,
    joined_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, member_kind, member_id)
);
CREATE INDEX conversation_members_member_idx ON conversation_members (member_kind, member_id);

CREATE TABLE messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    -- Monotonic in the conversation, without gaps: pagination and unread counts go by it.
    seq             bigint NOT NULL,
    author_kind     text NOT NULL CHECK (author_kind IN ('user', 'agent', 'system', 'plugin', 'external')),
    author_id       uuid,
    kind            text NOT NULL DEFAULT 'text' CHECK (kind IN ('text', 'card')),
    body            text NOT NULL DEFAULT '',        -- Markdown (a subset)
    card_type       text,
    -- {ref: {kind, id}, ...}: a card points at the object it shows; clients read its current state.
    card            jsonb,
    -- What a card is about, unique in its conversation ("job:<id>"): a card is updated, never posted twice.
    card_key        text,
    reply_count     integer NOT NULL DEFAULT 0,
    status          text NOT NULL DEFAULT 'final' CHECK (status IN ('streaming', 'final', 'failed')),
    origin          jsonb NOT NULL DEFAULT '{}'::jsonb,  -- e.g. {"source":"slack","ts":"…"} (phase 5)
    client_id       text,                                -- the sender's idempotency key
    edited_at       timestamptz,
    deleted_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'card') = (card_type IS NOT NULL)),
    CHECK (kind = 'card' OR (card IS NULL AND card_key IS NULL)),
    UNIQUE (conversation_id, seq),
    UNIQUE (conversation_id, client_id),
    UNIQUE (conversation_id, card_key)
);
ALTER TABLE conversations ADD CONSTRAINT conversations_parent_message_fk
    FOREIGN KEY (parent_message_id) REFERENCES messages(id) ON DELETE CASCADE;

CREATE TABLE message_mentions (
    message_id  uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    member_kind text NOT NULL CHECK (member_kind IN ('user', 'agent')),
    member_id   uuid NOT NULL,
    PRIMARY KEY (message_id, member_kind, member_id)
);
CREATE INDEX message_mentions_member_idx ON message_mentions (member_kind, member_id);

CREATE TABLE message_reactions (
    message_id  uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    member_kind text NOT NULL CHECK (member_kind IN ('user', 'agent')),
    member_id   uuid NOT NULL,
    emoji       text NOT NULL CHECK (length(emoji) BETWEEN 1 AND 64),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, member_kind, member_id, emoji)
);

-- A file is uploaded into a conversation first and belongs to no message until the message that
-- carries it is posted; message_id stays NULL until then.
CREATE TABLE message_attachments (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    message_id      uuid REFERENCES messages(id) ON DELETE CASCADE,
    uploaded_by     uuid REFERENCES users(id) ON DELETE SET NULL,
    name            text NOT NULL,
    content_type    text NOT NULL,
    size_bytes      bigint NOT NULL CHECK (size_bytes >= 0),
    storage_key     text NOT NULL UNIQUE,  -- an S3-compatible object or a file on local disk
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX message_attachments_message_idx ON message_attachments (message_id);

-- +goose Down
DROP TABLE message_attachments;
DROP TABLE message_reactions;
DROP TABLE message_mentions;
ALTER TABLE conversations DROP CONSTRAINT conversations_parent_message_fk;
DROP TABLE messages;
DROP TABLE conversation_members;
DROP TABLE conversations;
