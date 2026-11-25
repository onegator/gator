-- +goose Up
-- The Brain: one place for what a team and its agents know — about the product, about how it is
-- built, about its people and about what was decided. Every entry has a scope (who it is about and
-- who reads it), a status (an agent proposes, a person approves; only approved entries reach a
-- prompt), versions (a change is a new version, never an overwrite) and an origin.
CREATE TABLE brain_entries (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    scope          text NOT NULL CHECK (scope IN ('workspace', 'project', 'component', 'user', 'agent')),
    project_id     uuid REFERENCES projects(id) ON DELETE CASCADE,
    component_key  text,
    user_id        uuid REFERENCES users(id) ON DELETE CASCADE,
    agent_id       uuid REFERENCES agents(id) ON DELETE CASCADE,
    kind           text NOT NULL CHECK (kind IN (
                     'vision', 'principle', 'persona', 'glossary', 'decision', 'lesson', 'roadmap',
                     'convention', 'runbook', 'fact', 'expertise', 'focus', 'preference')),
    -- What a convention is about (language, architecture, requirements, testing, security): the
    -- shelves of the Knowledge screen.
    topic          text CHECK (topic IN ('language', 'architecture', 'requirements', 'testing', 'security')),
    title          text NOT NULL,
    content        text NOT NULL,
    -- 'scope' is the scope's own rule; a user's entry is its owner's unless it says otherwise.
    visibility     text NOT NULL DEFAULT 'scope' CHECK (visibility IN ('private', 'scope', 'project', 'workspace')),
    status         text NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed', 'approved', 'rejected', 'superseded', 'archived')),
    -- Every version of one entry shares its lineage (the first version's id).
    lineage        uuid NOT NULL,
    version        integer NOT NULL DEFAULT 1,
    supersedes     uuid REFERENCES brain_entries(id) ON DELETE SET NULL,
    valid_from     timestamptz NOT NULL DEFAULT now(),
    valid_until    timestamptz,
    confidence     text NOT NULL DEFAULT 'stated' CHECK (confidence IN ('stated', 'inferred')),
    -- {kind: message|task|job|plugin|manual|import|pack, id, url, excerpt}
    source         jsonb NOT NULL DEFAULT '{"kind":"manual"}'::jsonb,
    -- The task the entry came from, and the conversation; a proposal from a private conversation
    -- (a DM, a private channel) is read only by those who may read that conversation.
    source_task_id uuid REFERENCES tasks(id) ON DELETE SET NULL,
    source_conversation_id uuid REFERENCES conversations(id) ON DELETE SET NULL,
    source_private boolean NOT NULL DEFAULT false,
    -- Reading order among entries of one shelf.
    position       integer NOT NULL DEFAULT 100,
    -- A condensed entry names what it was made from; approving it archives those.
    condensed_from uuid[] NOT NULL DEFAULT '{}',
    archived_at    timestamptz,
    created_by_kind text NOT NULL DEFAULT 'system',
    created_by     uuid,
    approved_by    uuid REFERENCES users(id),
    approved_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CHECK ((scope = 'workspace') = (project_id IS NULL) OR scope = 'user'),
    CHECK (scope <> 'component' OR component_key IS NOT NULL),
    CHECK (scope <> 'user' OR user_id IS NOT NULL),
    CHECK (scope <> 'agent' OR agent_id IS NOT NULL),
    CHECK (visibility <> 'private' OR scope = 'user'),
    CHECK (visibility <> 'project' OR project_id IS NOT NULL)
);
-- Searched by words through an expression index, as messages are (00038): the same search without
-- a vector in every read of an entry.
CREATE INDEX brain_tsv_idx ON brain_entries USING gin (to_tsvector('simple', title || ' ' || content));
CREATE INDEX brain_scope_idx ON brain_entries (project_id, scope, kind, status);
-- One version number per lineage: two edits never both become version 3.
CREATE UNIQUE INDEX brain_lineage_idx ON brain_entries (lineage, version);
CREATE INDEX brain_user_idx ON brain_entries (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX brain_task_idx ON brain_entries (source_task_id) WHERE source_task_id IS NOT NULL;
-- One version number per entry, as product_context had per (project, kind, title). Conventions
-- carry no such promise: two workspace notes may share a title.
CREATE UNIQUE INDEX brain_version_idx ON brain_entries
    (scope, project_id, component_key, user_id, agent_id, kind, title, version) NULLS NOT DISTINCT
    WHERE topic IS NULL;

-- An entry's lineage is its own id unless it says otherwise.
-- +goose StatementBegin
CREATE FUNCTION gator_brain_lineage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS NULL THEN
        NEW.id := gen_random_uuid();
    END IF;
    IF NEW.lineage IS NULL THEN
        NEW.lineage := NEW.id;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER brain_entries_lineage BEFORE INSERT ON brain_entries
    FOR EACH ROW EXECUTE FUNCTION gator_brain_lineage();

-- What an entry is about besides itself: another entry, a component, a task, a conversation.
CREATE TABLE brain_links (
    entry_id    uuid NOT NULL REFERENCES brain_entries(id) ON DELETE CASCADE,
    target_kind text NOT NULL CHECK (target_kind IN ('entry', 'component', 'task', 'conversation')),
    target_id   text NOT NULL,
    relation    text NOT NULL DEFAULT 'relates' CHECK (relation IN ('relates', 'depends_on', 'contradicts', 'implements')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (entry_id, target_kind, target_id)
);
CREATE INDEX brain_links_target_idx ON brain_links (target_kind, target_id);

-- Which entries a job was handed, looked up or cited: "how does it know that", and which entries
-- nobody has used in a long time.
CREATE TABLE brain_citations (
    job_id     uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    entry_id   uuid NOT NULL REFERENCES brain_entries(id) ON DELETE CASCADE,
    how        text NOT NULL CHECK (how IN ('packed', 'searched', 'cited')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, entry_id, how)
);
CREATE INDEX brain_citations_entry_idx ON brain_citations (entry_id, created_at);

-- People who asked the Brain to forget them: their focus is not worked out again until they ask
-- for it.
CREATE TABLE brain_focus_off (
    user_id    uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    since      timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE brain_focus_off;
DROP TABLE brain_citations;
DROP TABLE brain_links;
DROP TABLE brain_entries;
DROP FUNCTION gator_brain_lineage();
