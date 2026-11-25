-- +goose Up
-- The product context and the knowledge entries move into the Brain, keeping their ids, statuses,
-- authors, versions and the tasks they came from. The old tables stay, renamed, until the next
-- release; under their old names are views over the Brain, so that anything still reading or
-- writing them in SQL finds the same rows. The down step moves every row back.

-- A product context's "principles" is the Brain's "principle"; every other kind keeps its name.
-- +goose StatementBegin
CREATE FUNCTION gator_brain_kind(product_kind text) RETURNS text LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE product_kind WHEN 'principles' THEN 'principle' ELSE product_kind END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION gator_product_kind(brain_kind text) RETURNS text LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE brain_kind WHEN 'principle' THEN 'principles' ELSE brain_kind END
$$;
-- +goose StatementEnd
-- The product context knew three statuses. A version a newer one replaced was still "approved"
-- there (the newest approved one was read), and turning a proposal down was archiving it.
-- +goose StatementBegin
CREATE FUNCTION gator_product_status(brain_status text) RETURNS text LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE brain_status WHEN 'superseded' THEN 'approved' WHEN 'rejected' THEN 'archived' ELSE brain_status END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION gator_legacy_source(task uuid, origin text) RETURNS jsonb LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE WHEN task IS NULL THEN jsonb_build_object('kind', 'import', 'name', origin)
                ELSE jsonb_build_object('kind', 'task', 'id', task::text) END
$$;
-- +goose StatementEnd

INSERT INTO brain_entries (id, scope, project_id, kind, title, content, visibility, status, lineage, version,
    supersedes, valid_from, source, source_task_id, condensed_from, archived_at, created_by_kind, created_by,
    approved_by, approved_at, created_at, updated_at)
SELECT p.id, 'project', p.project_id, gator_brain_kind(p.kind), p.title, p.content, 'scope', p.status,
    (SELECT f.id FROM product_context f WHERE f.project_id = p.project_id AND f.kind = p.kind AND f.title = p.title
     ORDER BY f.version, f.created_at LIMIT 1),
    p.version,
    (SELECT b.id FROM product_context b WHERE b.project_id = p.project_id AND b.kind = p.kind AND b.title = p.title
       AND b.version < p.version ORDER BY b.version DESC LIMIT 1),
    coalesce(p.approved_at, p.created_at), gator_legacy_source(p.source_task_id, 'product_context'), p.source_task_id,
    p.condensed_from, p.archived_at, p.created_by_kind, p.created_by, p.approved_by, p.approved_at, p.created_at, p.created_at
FROM product_context p
ORDER BY p.project_id, p.kind, p.title, p.version;

-- The product context read the newest approved version of each entry and left the older ones
-- approved; in the Brain one version of a lineage is true at a time. The older ones stopped being
-- true when the next version came; the views show them approved, as before, and the down step
-- gives them back as they were.
UPDATE brain_entries b SET status = 'superseded',
    valid_until = (SELECT coalesce(n.approved_at, n.created_at) FROM brain_entries n
                   WHERE n.lineage = b.lineage AND n.version > b.version ORDER BY n.version LIMIT 1)
WHERE b.status = 'approved' AND b.scope = 'project'
  AND EXISTS (SELECT 1 FROM brain_entries n WHERE n.lineage = b.lineage AND n.version > b.version AND n.status = 'approved');

INSERT INTO brain_entries (id, scope, project_id, kind, topic, title, content, visibility, status, lineage, version,
    valid_from, source, position, created_by_kind, created_at, updated_at)
SELECT k.id, CASE WHEN k.project_id IS NULL THEN 'workspace' ELSE 'project' END, k.project_id, 'convention', k.scope,
    k.title, k.content, 'scope', 'approved', k.id, 1, k.created_at, gator_legacy_source(NULL, 'knowledge_entries'),
    k.position, 'user', k.created_at, k.updated_at
FROM knowledge_entries k;

ALTER TABLE component_decisions DROP CONSTRAINT component_decisions_entry_id_fkey;
ALTER TABLE component_decisions ADD CONSTRAINT component_decisions_entry_id_fkey
    FOREIGN KEY (entry_id) REFERENCES brain_entries(id) ON DELETE CASCADE;

ALTER TABLE product_context RENAME TO product_context_legacy;
ALTER TABLE knowledge_entries RENAME TO knowledge_entries_legacy;

CREATE VIEW product_context AS
SELECT id, project_id, gator_product_kind(kind) AS kind, title, content, version, gator_product_status(status) AS status,
       source_task_id, created_by_kind, created_by, approved_by, approved_at, created_at, archived_at, condensed_from
FROM brain_entries
WHERE scope = 'project' AND kind IN ('vision', 'principle', 'persona', 'glossary', 'decision', 'lesson', 'roadmap');

CREATE VIEW knowledge_entries AS
SELECT id, project_id, topic AS scope, title, content, position, created_at, updated_at
FROM brain_entries
WHERE kind = 'convention' AND topic IS NOT NULL AND status = 'approved' AND scope IN ('workspace', 'project');

-- +goose StatementBegin
CREATE FUNCTION gator_product_context_write() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    brain_kind text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM brain_entries WHERE id = OLD.id;
        RETURN OLD;
    END IF;
    brain_kind := gator_brain_kind(NEW.kind);
    IF TG_OP = 'INSERT' THEN
        NEW.id := coalesce(NEW.id, gen_random_uuid());
        NEW.status := coalesce(NEW.status, 'proposed');
        NEW.created_by_kind := coalesce(NEW.created_by_kind, 'system');
        NEW.condensed_from := coalesce(NEW.condensed_from, '{}');
        NEW.created_at := coalesce(NEW.created_at, now());
        NEW.version := coalesce(NEW.version, (SELECT max(b.version) + 1 FROM brain_entries b
            WHERE b.scope = 'project' AND b.project_id = NEW.project_id AND b.kind = brain_kind AND b.title = NEW.title AND b.topic IS NULL), 1);
        INSERT INTO brain_entries (id, scope, project_id, kind, title, content, visibility, status, lineage, version,
            supersedes, valid_from, source, source_task_id, condensed_from, archived_at, created_by_kind, created_by,
            approved_by, approved_at, created_at, updated_at)
        VALUES (NEW.id, 'project', NEW.project_id, brain_kind, NEW.title, NEW.content, 'scope', NEW.status,
            coalesce((SELECT b.lineage FROM brain_entries b WHERE b.scope = 'project' AND b.project_id = NEW.project_id
                      AND b.kind = brain_kind AND b.title = NEW.title AND b.topic IS NULL ORDER BY b.version LIMIT 1), NEW.id),
            NEW.version,
            (SELECT b.id FROM brain_entries b WHERE b.scope = 'project' AND b.project_id = NEW.project_id AND b.kind = brain_kind
               AND b.title = NEW.title AND b.topic IS NULL AND b.version < NEW.version ORDER BY b.version DESC LIMIT 1),
            coalesce(NEW.approved_at, NEW.created_at), gator_legacy_source(NEW.source_task_id, 'product_context'),
            NEW.source_task_id, NEW.condensed_from, NEW.archived_at, NEW.created_by_kind, NEW.created_by,
            NEW.approved_by, NEW.approved_at, NEW.created_at, NEW.created_at);
        -- One version of a lineage is true at a time, however it was written.
        IF NEW.status = 'approved' THEN
            UPDATE brain_entries b SET status = 'superseded', valid_until = coalesce(b.valid_until, now())
            WHERE b.lineage = (SELECT lineage FROM brain_entries WHERE id = NEW.id)
              AND b.version < NEW.version AND b.status = 'approved';
        END IF;
        RETURN NEW;
    END IF;
    -- An UPDATE through the view changes the row and nothing else: no event, no approval effect
    -- (archiving what a condensate replaces, superseding what it contradicts). It is the old
    -- table's door for SQL; the API decides through brain.Service.
    UPDATE brain_entries SET project_id = NEW.project_id, kind = brain_kind, title = NEW.title, content = NEW.content,
        version = NEW.version,
        -- The view shows a replaced version as approved; writing that back changes nothing.
        status = CASE WHEN NEW.status = OLD.status THEN status ELSE NEW.status END,
        source_task_id = NEW.source_task_id, created_by_kind = NEW.created_by_kind, created_by = NEW.created_by,
        approved_by = NEW.approved_by, approved_at = NEW.approved_at, archived_at = NEW.archived_at,
        condensed_from = NEW.condensed_from, updated_at = now()
    WHERE id = OLD.id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER product_context_write INSTEAD OF INSERT OR UPDATE OR DELETE ON product_context
    FOR EACH ROW EXECUTE FUNCTION gator_product_context_write();

-- +goose StatementBegin
CREATE FUNCTION gator_knowledge_entries_write() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM brain_entries WHERE id = OLD.id;
        RETURN OLD;
    END IF;
    IF TG_OP = 'INSERT' THEN
        NEW.id := coalesce(NEW.id, gen_random_uuid());
        NEW.position := coalesce(NEW.position, 100);
        NEW.created_at := coalesce(NEW.created_at, now());
        NEW.updated_at := coalesce(NEW.updated_at, NEW.created_at);
        INSERT INTO brain_entries (id, scope, project_id, kind, topic, title, content, visibility, status, lineage,
            version, valid_from, source, position, created_by_kind, created_at, updated_at)
        VALUES (NEW.id, CASE WHEN NEW.project_id IS NULL THEN 'workspace' ELSE 'project' END, NEW.project_id,
            'convention', NEW.scope, NEW.title, NEW.content, 'scope', 'approved', NEW.id, 1, NEW.created_at,
            gator_legacy_source(NULL, 'knowledge_entries'), NEW.position, 'user', NEW.created_at, NEW.updated_at);
        RETURN NEW;
    END IF;
    UPDATE brain_entries SET scope = CASE WHEN NEW.project_id IS NULL THEN 'workspace' ELSE 'project' END,
        project_id = NEW.project_id, topic = NEW.scope, title = NEW.title, content = NEW.content,
        position = NEW.position, updated_at = coalesce(NEW.updated_at, now())
    WHERE id = OLD.id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER knowledge_entries_write INSTEAD OF INSERT OR UPDATE OR DELETE ON knowledge_entries
    FOR EACH ROW EXECUTE FUNCTION gator_knowledge_entries_write();

-- +goose Down
DROP TRIGGER knowledge_entries_write ON knowledge_entries;
DROP TRIGGER product_context_write ON product_context;
DROP FUNCTION gator_knowledge_entries_write();
DROP FUNCTION gator_product_context_write();
DROP VIEW knowledge_entries;
DROP VIEW product_context;
ALTER TABLE product_context_legacy RENAME TO product_context;
ALTER TABLE knowledge_entries_legacy RENAME TO knowledge_entries;

-- What the old tables hold is what the Brain holds now, written since or not.
ALTER TABLE component_decisions DROP CONSTRAINT component_decisions_entry_id_fkey;
DELETE FROM product_context;
DELETE FROM knowledge_entries;
INSERT INTO product_context (id, project_id, kind, title, content, version, status, source_task_id, created_by_kind,
    created_by, approved_by, approved_at, created_at, archived_at, condensed_from)
SELECT id, project_id, gator_product_kind(kind), title, content, version, gator_product_status(status), source_task_id,
    created_by_kind, created_by, approved_by, approved_at, created_at, archived_at, condensed_from
FROM brain_entries
WHERE scope = 'project' AND kind IN ('vision', 'principle', 'persona', 'glossary', 'decision', 'lesson', 'roadmap');
INSERT INTO knowledge_entries (id, project_id, scope, title, content, position, created_at, updated_at)
SELECT id, project_id, topic, title, content, position, created_at, updated_at
FROM brain_entries
WHERE kind = 'convention' AND topic IS NOT NULL AND status = 'approved' AND scope IN ('workspace', 'project');
DELETE FROM component_decisions WHERE entry_id NOT IN (SELECT id FROM product_context);
ALTER TABLE component_decisions ADD CONSTRAINT component_decisions_entry_id_fkey
    FOREIGN KEY (entry_id) REFERENCES product_context(id) ON DELETE CASCADE;
-- Each row lives in one place at every step of the migrations.
DELETE FROM brain_entries WHERE id IN (SELECT id FROM product_context UNION ALL SELECT id FROM knowledge_entries);

DROP FUNCTION gator_legacy_source(uuid, text);
DROP FUNCTION gator_product_status(text);
DROP FUNCTION gator_product_kind(text);
DROP FUNCTION gator_brain_kind(text);
