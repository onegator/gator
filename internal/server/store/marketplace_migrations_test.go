package store

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

// The marketplace's migrations go up over a database with work in it, down to the Brain's last
// one (00044, the head before them) and up again: what was there before is untouched each time,
// and the marketplace's own tables come and go whole.
func TestMarketplaceMigrationsRoundTrip(t *testing.T) {
	conn, pool := scratch(t)
	ctx := context.Background()
	const before = 44
	if err := goose.UpToContext(ctx, conn, "migrations", before); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	exec(`INSERT INTO users (id, email, name) VALUES ('00000000-0000-0000-0000-00000000f001', 'm@t.local', 'M')`)
	exec(`INSERT INTO projects (id, slug, name) VALUES ('00000000-0000-0000-0000-0000000000a1', 'alpha', 'Alpha')`)
	exec(`INSERT INTO plugins (name, command, version, manifest) VALUES ('echo', '{/bin/echo}', '1', '{}')`)
	agentsBefore := count(`SELECT count(*) FROM agents`)

	tables := func() int {
		return count(`SELECT count(*) FROM information_schema.tables
			WHERE table_name IN ('marketplace_registries', 'marketplace_items', 'installations', 'skills', 'mcp_servers', 'mcp_calls')`)
	}
	seed := func() {
		exec(`INSERT INTO marketplace_registries (id, name, url, public_key, builtin)
			VALUES ('00000000-0000-0000-0000-0000000e0001', 'gator', 'file:///r', 'ed25519:x', true)`)
		exec(`INSERT INTO marketplace_items (registry_id, kind, name, version, manifest, checksum)
			VALUES ('00000000-0000-0000-0000-0000000e0001', 'agent', 'qa-engineer', '1.2.0', '{}', 'sha256:00')`)
		exec(`INSERT INTO skills (project_id, name, version, description, files) VALUES ('00000000-0000-0000-0000-0000000000a1', 'go-testing', '1.0.0', 'd', '[]')`)
		exec(`INSERT INTO agents (id, project_id, handle, display_name, source) VALUES
			('00000000-0000-0000-0000-0000000d0001', '00000000-0000-0000-0000-0000000000a1', 'qa', 'QA', '{"kind":"marketplace"}')`)
		exec(`INSERT INTO installations (project_id, registry_id, kind, name, version, checksum, target_id, accepted_permissions, installed_by)
			VALUES ('00000000-0000-0000-0000-0000000000a1', '00000000-0000-0000-0000-0000000e0001', 'agent', 'qa-engineer', '1.2.0', 'sha256:00',
			        '00000000-0000-0000-0000-0000000d0001', '["reads CI logs"]', '00000000-0000-0000-0000-00000000f001')`)
		exec(`INSERT INTO mcp_servers (id, project_id, name, url, auth, allowed_tools)
			VALUES ('00000000-0000-0000-0000-0000000c0001', '00000000-0000-0000-0000-0000000000a1', 'sentry', 'https://mcp.sentry.dev/mcp', 'oauth', '{search_issues}')`)
		exec(`INSERT INTO mcp_calls (mcp_server_id, project_id, method, tool, outcome)
			VALUES ('00000000-0000-0000-0000-0000000c0001', '00000000-0000-0000-0000-0000000000a1', 'tools/call', 'search_issues', 'ok')`)
	}

	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	if tables() == 0 {
		t.Fatal("up made no marketplace tables")
	}
	seed()
	// The calls are a record: nobody changes one, but a server going takes its calls with it.
	if _, err := pool.Exec(ctx, `UPDATE mcp_calls SET outcome = 'error'`); err == nil {
		t.Fatal("a call was changed")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM mcp_calls`); err == nil {
		t.Fatal("a call was deleted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO mcp_servers (name, url, auth) VALUES ('gator', 'https://x', 'none')`); err == nil {
		t.Fatal("a remote server named gator")
	}
	exec(`DELETE FROM mcp_servers WHERE name = 'sentry'`)
	if count(`SELECT count(*) FROM mcp_calls`) != 0 {
		t.Fatal("a server's calls go with it")
	}
	exec(`INSERT INTO mcp_servers (id, project_id, name, url, auth) VALUES ('00000000-0000-0000-0000-0000000c0001', '00000000-0000-0000-0000-0000000000a1', 'sentry', 'https://x', 'none')`)
	exec(`INSERT INTO mcp_calls (mcp_server_id, method, outcome) VALUES ('00000000-0000-0000-0000-0000000c0001', 'tools/list', 'ok')`)
	if err := goose.DownToContext(ctx, conn, "migrations", before); err != nil {
		t.Fatal(err)
	}
	if n := tables(); n != 0 {
		t.Fatalf("down left %d marketplace tables", n)
	}
	// What was there before is still there; what was installed stays a project's agent.
	if count(`SELECT count(*) FROM projects`) != 1 || count(`SELECT count(*) FROM plugins`) != 1 || count(`SELECT count(*) FROM agents`) != agentsBefore+1 {
		t.Fatal("down took what it did not make")
	}
	exec(`DELETE FROM agents WHERE handle = 'qa'`)
	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	seed()
	if count(`SELECT count(*) FROM installations`) != 1 {
		t.Fatal("second up")
	}
}
