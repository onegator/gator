package store

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

// Phase 5's migrations (00048 bridges, 00049 A2A) go up over a database with conversations and
// turns in it, down to the marketplace's last one (00047) and up again: what was there before is
// untouched, and their own tables and columns come and go whole.
func TestExternalMigrationsRoundTrip(t *testing.T) {
	conn, pool := scratch(t)
	ctx := context.Background()
	const before = 47
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
	const (
		user    = "00000000-0000-0000-0000-00000000f001"
		project = "00000000-0000-0000-0000-0000000000a1"
		channel = "00000000-0000-0000-0000-0000000000c1"
		ask     = "00000000-0000-0000-0000-0000000000e1"
	)
	exec(`INSERT INTO users (id, email, name) VALUES ($1, 'm@t.local', 'M')`, user)
	exec(`INSERT INTO projects (id, slug, name) VALUES ($1, 'alpha', 'Alpha')`, project)
	exec(`INSERT INTO conversations (id, project_id, kind, name, visibility, created_by_kind) VALUES ($1, $2, 'channel', 'eng', 'project', 'system')`, channel, project)
	exec(`INSERT INTO messages (id, conversation_id, seq, author_kind, author_id, kind, body, origin, status)
		VALUES ($1, $2, 1, 'user', $3, 'text', 'hello', '{}', 'final')`, ask, channel, user)
	exec(`INSERT INTO agent_turns (agent_id, project_id, conversation_id, trigger_message_id, requested_by_kind, requested_by, tier, status, chain_id)
		SELECT id, $1, $2, $3, 'user', $4, 'chat', 'done', gen_random_uuid() FROM agents WHERE project_id = $1 AND handle = 'reviewer'`, project, channel, ask, user)
	agentsBefore, turnsBefore := count(`SELECT count(*) FROM agents`), count(`SELECT count(*) FROM agent_turns`)
	tables := func() int {
		return count(`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('external_identities',
			'external_conversation_refs', 'external_message_refs', 'external_link_codes', 'a2a_clients', 'a2a_tasks', 'a2a_calls')`)
	}
	columns := func() int {
		return count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'agents'
			AND column_name IN ('kind', 'remote', 'remote_secrets', 'a2a_exposed')`)
	}
	seed := func() {
		t.Helper()
		exec(`INSERT INTO external_identities (id, project_id, provider, external_id, display_name)
			VALUES ('00000000-0000-0000-0000-0000000001d1', $1, 'slack', 'T1/U1', 'Ada')`, project)
		exec(`INSERT INTO external_conversation_refs (project_id, provider, external_ref, conversation_id) VALUES ($1, 'slack', 'T1/C1', $2)`, project, channel)
		exec(`INSERT INTO messages (id, conversation_id, seq, author_kind, author_id, kind, body, origin, status)
			VALUES ('00000000-0000-0000-0000-0000000000e2', $1, 2, 'external', '00000000-0000-0000-0000-0000000001d1', 'text', 'from slack',
			        '{"provider":"slack","external_ref":"T1/C1/1.1"}', 'final')`, channel)
		exec(`INSERT INTO external_message_refs (project_id, provider, external_ref, message_id) VALUES ($1, 'slack', 'T1/C1/1.1', '00000000-0000-0000-0000-0000000000e2')`, project)
		exec(`INSERT INTO external_link_codes (hash, project_id, provider, user_id, expires_at) VALUES ('\x01', $1, 'slack', $2, now())`, project, user)
		exec(`INSERT INTO agent_turns (agent_id, project_id, conversation_id, trigger_message_id, requested_by_kind, requested_by, tier, status, chain_id)
			SELECT id, $1, $2, '00000000-0000-0000-0000-0000000000e2', 'external', '00000000-0000-0000-0000-0000000001d1', 'chat', 'done', gen_random_uuid()
			FROM agents WHERE project_id = $1 AND handle = 'reviewer'`, project, channel)
		exec(`INSERT INTO agents (id, project_id, handle, display_name, kind, remote, remote_secrets, source)
			VALUES ('00000000-0000-0000-0000-0000000d0001', $1, 'vendor', 'Vendor', 'remote', '{"url":"https://v"}', 'k1:sealed', '{"kind":"custom"}')`, project)
		exec(`INSERT INTO agent_turns (id, agent_id, project_id, conversation_id, trigger_message_id, requested_by_kind, requested_by, tier, status, chain_id)
			VALUES ('00000000-0000-0000-0000-0000000007a1', '00000000-0000-0000-0000-0000000d0001', $1, $2, $3, 'user', $4, 'chat', 'running', gen_random_uuid())`,
			project, channel, ask, user)
		exec(`INSERT INTO a2a_calls (turn_id, agent_id, project_id) VALUES ('00000000-0000-0000-0000-0000000007a1', '00000000-0000-0000-0000-0000000d0001', $1)`, project)
		exec(`INSERT INTO api_tokens (id, kind, scope, hash, project_id) VALUES ('00000000-0000-0000-0000-0000000070a1', 'a2a', 'a2a:partner', '\x02', $1)`, project)
		exec(`INSERT INTO external_identities (id, project_id, provider, external_id, display_name)
			VALUES ('00000000-0000-0000-0000-0000000001d2', $1, 'a2a', 'client/partner', 'partner')`, project)
		exec(`INSERT INTO a2a_clients (id, project_id, name, token_id, identity_id)
			VALUES ('00000000-0000-0000-0000-0000000c1a01', $1, 'partner', '00000000-0000-0000-0000-0000000070a1', '00000000-0000-0000-0000-0000000001d2')`, project)
		exec(`INSERT INTO a2a_tasks (client_id, project_id, agent_id, message_id)
			SELECT '00000000-0000-0000-0000-0000000c1a01', $1, id, '00000000-0000-0000-0000-0000000000e2' FROM agents WHERE project_id = $1 AND handle = 'reviewer'`, project)
		exec(`UPDATE agents SET a2a_exposed = true WHERE project_id = $1 AND handle = 'reviewer'`, project)
	}

	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	if tables() != 7 || columns() != 4 {
		t.Fatalf("up made %d tables and %d agent columns", tables(), columns())
	}
	indexes := func() int {
		return count(`SELECT count(*) FROM pg_indexes WHERE indexname IN ('agent_turns_external_idx', 'agent_turns_parent_idx',
			'external_conversation_refs_conversation_idx')`)
	}
	if indexes() != 3 {
		t.Fatalf("up made %d of the outside budget's and bridges' indexes", indexes())
	}
	seed()
	// What the constraints keep: a remote agent chats, plays no role and is no one's to expose.
	for _, bad := range []string{
		`UPDATE agents SET tier = 'work' WHERE handle = 'vendor'`,
		`UPDATE agents SET a2a_exposed = true WHERE handle = 'vendor'`,
		`INSERT INTO agents (handle, display_name, kind) VALUES ('nowhere', 'N', 'remote')`,
		`INSERT INTO external_identities (project_id, provider, external_id, display_name, user_id) VALUES ('` + project + `', 'slack', 'T1/U9', '', '` + user + `')`,
		`INSERT INTO external_identities (project_id, provider, external_id) VALUES ('` + project + `', 'slack', 'T1/U1')`,
	} {
		if _, err := pool.Exec(ctx, bad); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	if err := goose.DownToContext(ctx, conn, "migrations", before); err != nil {
		t.Fatal(err)
	}
	if n, c := tables(), columns(); n != 0 || c != 0 || indexes() != 0 {
		t.Fatalf("down left %d tables and %d agent columns", n, c)
	}
	// What was there before is still there; a turn asked from outside stays, as the process's.
	if count(`SELECT count(*) FROM projects`) != 1 || count(`SELECT count(*) FROM agents`) != agentsBefore ||
		count(`SELECT count(*) FROM agent_turns`) != turnsBefore+1 || count(`SELECT count(*) FROM agent_turns WHERE requested_by_kind = 'process'`) != 1 ||
		count(`SELECT count(*) FROM messages`) != 2 || count(`SELECT count(*) FROM api_tokens WHERE kind = 'a2a'`) != 0 {
		t.Fatal("down took what it did not make, or left what it did")
	}
	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	exec(`DELETE FROM agent_turns WHERE requested_by_kind = 'process'`)
	exec(`DELETE FROM messages WHERE id = '00000000-0000-0000-0000-0000000000e2'`)
	seed()
	if tables() != 7 || count(`SELECT count(*) FROM a2a_tasks`) != 1 {
		t.Fatal("second up")
	}
}
