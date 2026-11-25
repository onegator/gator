-- +goose Up
-- An agent people made is put away, not erased: its turns are the record of what it did and what
-- it cost, and a turn still answering must be able to finish. A handle put away can be taken again.
ALTER TABLE agents ADD COLUMN deleted_at timestamptz;
DROP INDEX agents_handle_idx;
CREATE UNIQUE INDEX agents_handle_idx ON agents (project_id, handle) NULLS NOT DISTINCT WHERE deleted_at IS NULL;
ALTER TABLE agent_turns
    DROP CONSTRAINT agent_turns_agent_id_fkey,
    ADD CONSTRAINT agent_turns_agent_id_fkey FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE RESTRICT;

-- A turn being started is claimed, so two starts of one turn (a redelivered mention, two servers)
-- never both queue a job. A claim nobody finished (a crash) expires and the next delivery retries.
ALTER TABLE agent_turns ADD COLUMN claimed_at timestamptz;

-- Locks on a handle: a project's agent takes the handle shared and its project's slot exclusive,
-- so projects creating their defaults do not wait on each other; a workspace agent, seen
-- everywhere, takes the handle exclusive.
-- +goose StatementBegin
CREATE FUNCTION gator_lock_handle(h text, p uuid) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF p IS NULL THEN
        PERFORM pg_advisory_xact_lock(hashtextextended('agent-handle:' || h, 0));
    ELSE
        PERFORM pg_advisory_xact_lock_shared(hashtextextended('agent-handle:' || h, 0));
        PERFORM pg_advisory_xact_lock(hashtextextended('agent-handle:' || h || ':' || p::text, 0));
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION agents_handle_visible_once() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.deleted_at IS NOT NULL THEN
        RETURN NEW;
    END IF;
    PERFORM gator_lock_handle(NEW.handle, NEW.project_id);
    IF EXISTS (
        SELECT 1 FROM agents a
        WHERE a.handle = NEW.handle AND a.id <> NEW.id AND a.deleted_at IS NULL
          AND (NEW.project_id IS NULL OR a.project_id IS NULL OR a.project_id = NEW.project_id)
    ) THEN
        RAISE EXCEPTION 'the handle @% is already taken where it would be seen', NEW.handle
            USING ERRCODE = 'unique_violation', CONSTRAINT = 'agents_handle_idx';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
DROP TRIGGER agents_handle_visible_once ON agents;
CREATE TRIGGER agents_handle_visible_once BEFORE INSERT OR UPDATE OF handle, project_id, deleted_at ON agents
    FOR EACH ROW EXECUTE FUNCTION agents_handle_visible_once();

-- The defaults are seeded one by one under the handle's lock, so a workspace agent made at the
-- same moment makes the default step aside instead of failing the project's creation.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION gator_seed_default_agents(p uuid) RETURNS void LANGUAGE plpgsql AS $$
DECLARE d record;
BEGIN
    FOR d IN SELECT * FROM (VALUES
        ('researcher', 'Researcher', 'Finds out what a task is really about before anyone plans it.', 'magnifyingglass', 'researcher', 'chat'),
        ('planner',    'Planner',    'Turns a brief into a plan somebody can build and review.',      'list.bullet.clipboard', 'planner', 'chat'),
        ('worker',     'Worker',     'Changes the code on a gator/ branch and commits it.',            'hammer', 'worker', 'work'),
        ('reviewer',   'Reviewer',   'Reads a change and says what is wrong with it.',                 'checkmark.seal', 'reviewer', 'chat'),
        ('security',   'Security',   'Reads a change for what an attacker could use.',                 'lock.shield', 'security_reviewer', 'chat'),
        ('curator',    'Curator',    'Keeps what the project has learned short and true.',             'books.vertical', 'curator', 'chat'),
        ('triage',     'Triage',     'Looks into an error from production and finds its cause.',       'stethoscope', 'triage', 'work'),
        ('reporter',   'Reporter',   'Says what happened in the project, in a few lines.',             'newspaper', 'reporter', 'chat')
    ) AS v(handle, display_name, description, avatar, role_key, tier)
    LOOP
        PERFORM gator_lock_handle(d.handle, p);
        IF NOT EXISTS (SELECT 1 FROM agents a WHERE a.handle = d.handle AND a.deleted_at IS NULL
                       AND (a.project_id = p OR a.project_id IS NULL)) THEN
            INSERT INTO agents (project_id, handle, display_name, description, avatar, role_key, tier)
            VALUES (p, d.handle, d.display_name, d.description, d.avatar, d.role_key, d.tier)
            ON CONFLICT DO NOTHING;
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION gator_seed_default_agents(p uuid) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO agents (project_id, handle, display_name, description, avatar, role_key, tier)
    SELECT p, d.handle, d.display_name, d.description, d.avatar, d.role_key, d.tier
    FROM (VALUES
        ('researcher', 'Researcher', 'Finds out what a task is really about before anyone plans it.', 'magnifyingglass', 'researcher', 'chat'),
        ('planner',    'Planner',    'Turns a brief into a plan somebody can build and review.',      'list.bullet.clipboard', 'planner', 'chat'),
        ('worker',     'Worker',     'Changes the code on a gator/ branch and commits it.',            'hammer', 'worker', 'work'),
        ('reviewer',   'Reviewer',   'Reads a change and says what is wrong with it.',                 'checkmark.seal', 'reviewer', 'chat'),
        ('security',   'Security',   'Reads a change for what an attacker could use.',                 'lock.shield', 'security_reviewer', 'chat'),
        ('curator',    'Curator',    'Keeps what the project has learned short and true.',             'books.vertical', 'curator', 'chat'),
        ('triage',     'Triage',     'Looks into an error from production and finds its cause.',       'stethoscope', 'triage', 'work'),
        ('reporter',   'Reporter',   'Says what happened in the project, in a few lines.',             'newspaper', 'reporter', 'chat')
    ) AS d(handle, display_name, description, avatar, role_key, tier)
    WHERE NOT EXISTS (SELECT 1 FROM agents a WHERE a.handle = d.handle AND (a.project_id = p OR a.project_id IS NULL))
    ON CONFLICT DO NOTHING;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION agents_handle_visible_once() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('agent-handle:' || NEW.handle, 0));
    IF EXISTS (
        SELECT 1 FROM agents a
        WHERE a.handle = NEW.handle AND a.id <> NEW.id
          AND (NEW.project_id IS NULL OR a.project_id IS NULL OR a.project_id = NEW.project_id)
    ) THEN
        RAISE EXCEPTION 'the handle @% is already taken where it would be seen', NEW.handle
            USING ERRCODE = 'unique_violation', CONSTRAINT = 'agents_handle_idx';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
DROP TRIGGER agents_handle_visible_once ON agents;
CREATE TRIGGER agents_handle_visible_once BEFORE INSERT OR UPDATE OF handle, project_id ON agents
    FOR EACH ROW EXECUTE FUNCTION agents_handle_visible_once();
DROP FUNCTION gator_lock_handle(text, uuid);
ALTER TABLE agent_turns DROP COLUMN claimed_at;
ALTER TABLE agent_turns
    DROP CONSTRAINT agent_turns_agent_id_fkey,
    ADD CONSTRAINT agent_turns_agent_id_fkey FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE;
DELETE FROM agents WHERE deleted_at IS NOT NULL;
DROP INDEX agents_handle_idx;
CREATE UNIQUE INDEX agents_handle_idx ON agents (project_id, handle) NULLS NOT DISTINCT;
ALTER TABLE agents DROP COLUMN deleted_at;
