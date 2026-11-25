package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

// snapshot reads tables whole, in a fixed order, as JSON: what "the same data" means.
func snapshot(t *testing.T, pool *pgxpool.Pool, queries map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, q := range queries {
		var s *string
		if err := pool.QueryRow(context.Background(), `SELECT json_agg(r ORDER BY r.id)::text FROM (`+q+`) r`).Scan(&s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s != nil {
			out[name] = *s
		}
	}
	return out
}

// The Brain's migrations go up over a database with a product context, knowledge entries, tasks
// and jobs in it, down to where they started and up again. The old rows come back exactly as they
// were every time, and while the Brain holds them, the old names still read and write them.
func TestBrainMigrationsRoundTrip(t *testing.T) {
	conn, pool := scratch(t)
	ctx := context.Background()
	const before = 41
	if err := goose.UpToContext(ctx, conn, "migrations", before); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, email, name) VALUES ('00000000-0000-0000-0000-00000000f001', 'ada@t.local', 'Ada')`)
	exec(`INSERT INTO projects (id, slug, name) VALUES
		('00000000-0000-0000-0000-0000000000a1', 'alpha', 'Alpha'),
		('00000000-0000-0000-0000-0000000000b2', 'beta', 'Beta')`)
	exec(`INSERT INTO tasks (id, project_id, kind, title, phase) VALUES
		('00000000-0000-0000-0000-00000000aa01', '00000000-0000-0000-0000-0000000000a1', 'feature', 'A', 'planning'),
		('00000000-0000-0000-0000-00000000bb01', '00000000-0000-0000-0000-0000000000b2', 'bug', 'B', 'implementation')`)
	exec(`INSERT INTO jobs (id, task_id, project_id, phase, role, backend, context_bytes, context_docs) VALUES
		('00000000-0000-0000-0000-0000000a0001', '00000000-0000-0000-0000-00000000aa01', '00000000-0000-0000-0000-0000000000a1', 'planning', 'planner', 'claude', 1200, 3),
		('00000000-0000-0000-0000-0000000b0001', '00000000-0000-0000-0000-00000000bb01', '00000000-0000-0000-0000-0000000000b2', 'implementation', 'worker', 'claude', 800, 2)`)
	exec(`INSERT INTO product_context (id, project_id, kind, title, content, version, status, source_task_id, created_by_kind, created_by, approved_by, approved_at, created_at) VALUES
		('00000000-0000-0000-0000-00000000c001', '00000000-0000-0000-0000-0000000000a1', 'vision', 'Why', 'Calm at night.', 1, 'approved', NULL, 'user', '00000000-0000-0000-0000-00000000f001', '00000000-0000-0000-0000-00000000f001', '2026-01-02 10:00:00+00', '2026-01-01 10:00:00+00'),
		('00000000-0000-0000-0000-00000000c002', '00000000-0000-0000-0000-0000000000a1', 'vision', 'Why', 'Calm at night, and fast.', 2, 'approved', NULL, 'user', NULL, NULL, '2026-02-02 10:00:00+00', '2026-02-01 10:00:00+00'),
		('00000000-0000-0000-0000-00000000c003', '00000000-0000-0000-0000-0000000000a1', 'principles', 'Small steps', 'One change at a time.', 1, 'proposed', '00000000-0000-0000-0000-00000000aa01', 'runner', NULL, NULL, NULL, '2026-03-01 10:00:00+00'),
		('00000000-0000-0000-0000-00000000c004', '00000000-0000-0000-0000-0000000000a1', 'lesson', 'Cache', 'Keys need the locale.', 1, 'archived', '00000000-0000-0000-0000-00000000aa01', 'system', NULL, NULL, NULL, '2026-03-02 10:00:00+00'),
		('00000000-0000-0000-0000-00000000c005', '00000000-0000-0000-0000-0000000000b2', 'decision', 'Postgres', 'We use Postgres.', 1, 'approved', '00000000-0000-0000-0000-00000000bb01', 'system', NULL, NULL, '2026-03-03 10:00:00+00', '2026-03-03 09:00:00+00')`)
	exec(`UPDATE product_context SET archived_at = '2026-04-01 10:00:00+00' WHERE id = '00000000-0000-0000-0000-00000000c004'`)
	exec(`INSERT INTO product_context (id, project_id, kind, title, content, version, status, created_by_kind, condensed_from, created_at) VALUES
		('00000000-0000-0000-0000-00000000c006', '00000000-0000-0000-0000-0000000000a1', 'lesson', 'Condense the lesson entries', 'Keys need the locale; tests need the DB.', 1, 'proposed', 'system', '{00000000-0000-0000-0000-00000000c004}', '2026-04-01 10:00:00+00')`)
	exec(`INSERT INTO knowledge_entries (id, project_id, scope, title, content, position, created_at, updated_at) VALUES
		('00000000-0000-0000-0000-00000000d001', NULL, 'language', 'Go', 'Wrap errors with %w.', 10, '2026-01-01 10:00:00+00', '2026-01-05 10:00:00+00'),
		('00000000-0000-0000-0000-00000000d002', NULL, 'language', 'Go', 'A second note with the same title.', 20, '2026-01-01 11:00:00+00', '2026-01-01 11:00:00+00'),
		('00000000-0000-0000-0000-00000000d003', '00000000-0000-0000-0000-0000000000a1', 'security', 'Secrets', 'Never in logs.', 100, '2026-01-02 10:00:00+00', '2026-01-02 10:00:00+00')`)
	exec(`INSERT INTO components (id, project_id, key, name) VALUES ('00000000-0000-0000-0000-00000000e001', '00000000-0000-0000-0000-0000000000b2', 'db', 'Database')`)
	exec(`INSERT INTO component_decisions (component_id, entry_id) VALUES ('00000000-0000-0000-0000-00000000e001', '00000000-0000-0000-0000-00000000c005')`)

	tables := map[string]string{
		"product_context":     `SELECT * FROM product_context`,
		"knowledge_entries":   `SELECT * FROM knowledge_entries`,
		"tasks":               `SELECT id, project_id, kind, title, phase, component_id FROM tasks`,
		"jobs":                `SELECT id, task_id, project_id, phase, role, context_bytes, context_docs, kind, tier FROM jobs`,
		"component_decisions": `SELECT component_id AS id, entry_id FROM component_decisions`,
	}
	want := snapshot(t, pool, tables)
	same := func(round string) {
		t.Helper()
		got := snapshot(t, pool, tables)
		for name, w := range want {
			if got[name] != w {
				t.Fatalf("%s: %s changed\nwant %s\n got %s", round, name, w, got[name])
			}
		}
	}

	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	same("first up")
	var entries, lineages int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT lineage) FROM brain_entries`).Scan(&entries, &lineages); err != nil {
		t.Fatal(err)
	}
	// Six product entries in five lineages (two versions of the vision), three conventions.
	if entries != 9 || lineages != 8 {
		t.Fatalf("the Brain holds %d entries in %d lineages", entries, lineages)
	}
	var kind, source string
	if err := pool.QueryRow(ctx, `SELECT kind, source->>'kind' FROM brain_entries WHERE id = '00000000-0000-0000-0000-00000000c003'`).Scan(&kind, &source); err != nil || kind != "principle" || source != "task" {
		t.Fatalf("principles is the Brain's principle, from its task: %s %s %v", kind, source, err)
	}
	var supersedes string
	if err := pool.QueryRow(ctx, `SELECT supersedes::text FROM brain_entries WHERE id = '00000000-0000-0000-0000-00000000c002'`).Scan(&supersedes); err != nil || supersedes != "00000000-0000-0000-0000-00000000c001" {
		t.Fatalf("the second version follows the first: %s %v", supersedes, err)
	}
	// The old tables are kept, as they were.
	var kept int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM product_context_legacy) + (SELECT count(*) FROM knowledge_entries_legacy)`).Scan(&kept); err != nil || kept != 9 {
		t.Fatalf("the old tables stay for a release: %d %v", kept, err)
	}

	if err := goose.DownToContext(ctx, conn, "migrations", before); err != nil {
		t.Fatal(err)
	}
	same("down")
	var brainLeft int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name LIKE 'brain_%' OR table_name LIKE '%_legacy'`).Scan(&brainLeft); err != nil || brainLeft != 0 {
		t.Fatalf("down left %d tables behind (%v)", brainLeft, err)
	}

	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	same("second up")

	// Under the Brain, the old names still write: a new version through the view is the next
	// version of its lineage, and goes back with everything else.
	exec(`INSERT INTO product_context (project_id, kind, title, content, version, status, created_by_kind, approved_at)
		VALUES ('00000000-0000-0000-0000-0000000000a1', 'vision', 'Why', 'Calm, fast, kind.', 3, 'approved', 'user', now())`)
	exec(`INSERT INTO knowledge_entries (project_id, scope, title, content) VALUES (NULL, 'testing', 'Table tests', 'Prefer them.')`)
	var lineage string
	if err := pool.QueryRow(ctx, `SELECT lineage::text FROM brain_entries WHERE title = 'Why' AND version = 3`).Scan(&lineage); err != nil || lineage != "00000000-0000-0000-0000-00000000c001" {
		t.Fatalf("a version written through the view joins its lineage: %s %v", lineage, err)
	}
	if err := goose.DownToContext(ctx, conn, "migrations", before); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM product_context) + (SELECT count(*) FROM knowledge_entries)`).Scan(&rows); err != nil || rows != 11 {
		t.Fatalf("what was written under the Brain goes back too: %d %v", rows, err)
	}
}
