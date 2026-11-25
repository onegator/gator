-- +goose Up
-- An agent is a member of the team: a handle people mention (@reviewer), a profile, a role in the
-- process, a tier of what it may do, a budget and a state. The roles of today's process become the
-- default agents of every project; a project adds its own, or one for the whole workspace
-- (project_id NULL) serves every project.
CREATE TABLE agents (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid REFERENCES projects(id) ON DELETE CASCADE,
    -- What people type after @. Lower case, digits and dashes, 2–32 long, not starting with a dash.
    handle        text NOT NULL CHECK (handle ~ '^[a-z0-9][a-z0-9-]{1,31}$'),
    display_name  text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 80),
    description   text NOT NULL DEFAULT '',            -- one line, shown in @autocomplete
    avatar        text NOT NULL DEFAULT '',            -- an SF Symbol, an emoji or a url
    role_key      text,                                -- the process role it plays (templates say role: reviewer)
    instructions  text NOT NULL DEFAULT '',            -- empty: the role's prompt (process/roles)
    tier          text NOT NULL DEFAULT 'chat' CHECK (tier IN ('chat', 'work')),
    policy        jsonb NOT NULL DEFAULT '{}',         -- {backend, model, max_cost_usd}; empty: the project's policy
    tools         jsonb NOT NULL DEFAULT '[]',         -- MCP servers and plugins (phase 4)
    skills        jsonb NOT NULL DEFAULT '[]',         -- phase 4
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused')),
    source        jsonb NOT NULL DEFAULT '{"kind":"builtin"}'
                  CHECK (source->>'kind' IN ('builtin', 'custom', 'marketplace')),
    created_by    uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
-- One handle per scope; the workspace (NULL) is one scope. The trigger below adds the rule an index
-- cannot say: a handle visible in a project is the project's or the workspace's, never both.
CREATE UNIQUE INDEX agents_handle_idx ON agents (project_id, handle) NULLS NOT DISTINCT;
-- One built-in agent per role and scope, so the backfill and new projects can be repeated safely.
CREATE UNIQUE INDEX agents_builtin_role_idx ON agents (project_id, role_key) NULLS NOT DISTINCT
    WHERE source->>'kind' = 'builtin';
CREATE INDEX agents_role_idx ON agents (role_key) WHERE role_key IS NOT NULL;

-- A mention names a handle, and in a project it has to mean one agent. The check takes a lock on
-- the handle first, so two people creating @x at once (one in a project, one for the workspace)
-- take turns and the second one sees the first. Under READ COMMITTED every statement of a volatile
-- function reads a fresh snapshot, so the query after the lock sees a row committed while it waited.
-- +goose StatementBegin
CREATE FUNCTION agents_handle_visible_once() RETURNS trigger LANGUAGE plpgsql AS $$
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
CREATE TRIGGER agents_handle_visible_once BEFORE INSERT OR UPDATE OF handle, project_id ON agents
    FOR EACH ROW EXECUTE FUNCTION agents_handle_visible_once();

-- The default agents of a project: one per role of the process, plus @triage and @reporter, the
-- authors of the error-triage and weekly-summary automations. A handle a workspace agent already
-- has is skipped (that agent answers to it), and so is a role the project already has a built-in
-- for, so this can run again. Their prompt is the role's (instructions empty).
-- +goose StatementBegin
CREATE FUNCTION gator_seed_default_agents(p uuid) RETURNS void LANGUAGE sql AS $$
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
$$;
-- +goose StatementEnd

-- Every project has them from its first moment, however it was made.
-- +goose StatementBegin
CREATE FUNCTION projects_default_agents() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM gator_seed_default_agents(NEW.id);
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER projects_default_agents AFTER INSERT ON projects
    FOR EACH ROW EXECUTE FUNCTION projects_default_agents();

SELECT gator_seed_default_agents(id) FROM projects;

-- A job is an agent's work. Jobs made before agents existed belong to the built-in agent of their
-- role in their project.
ALTER TABLE jobs ADD COLUMN agent_id uuid REFERENCES agents(id) ON DELETE SET NULL;
UPDATE jobs j SET agent_id = a.id
FROM agents a
WHERE a.project_id = j.project_id AND a.role_key = j.role AND a.source->>'kind' = 'builtin';
CREATE INDEX jobs_agent_idx ON jobs (agent_id, created_at) WHERE agent_id IS NOT NULL;

-- +goose Down
DROP INDEX jobs_agent_idx;
ALTER TABLE jobs DROP COLUMN agent_id;
DROP TRIGGER projects_default_agents ON projects;
DROP FUNCTION projects_default_agents();
DROP FUNCTION gator_seed_default_agents(uuid);
DROP TABLE agents;
DROP FUNCTION agents_handle_visible_once();
