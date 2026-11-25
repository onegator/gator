package store

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

// Phase 7's migrations (00050 checkpoints, 00051 review comments, 00052 runtime requests, 00053
// message intent, 00054 settling) go up over a database with a job and a conversation in it,
// take their own data, go down to phase 5's last one (00049) and up again: what was there before
// is untouched, a job left settling goes back to running, and their tables and columns come and
// go whole.
func TestReviewMigrationsRoundTrip(t *testing.T) {
	conn, pool := scratch(t)
	ctx := context.Background()
	const before = 49
	if err := goose.UpToContext(ctx, conn, "migrations", before); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	const (
		user    = "00000000-0000-0000-0000-00000000f001"
		project = "00000000-0000-0000-0000-0000000000a1"
		task    = "00000000-0000-0000-0000-0000000000b1"
		job     = "00000000-0000-0000-0000-0000000000c1"
		channel = "00000000-0000-0000-0000-0000000000d1"
		message = "00000000-0000-0000-0000-0000000000e1"
	)
	exec(`INSERT INTO users (id, email, name) VALUES ($1, 'm@t.local', 'M')`, user)
	exec(`INSERT INTO projects (id, slug, name) VALUES ($1, 'alpha', 'Alpha')`, project)
	exec(`INSERT INTO tasks (id, project_id, kind, title, phase) VALUES ($1, $2, 'bug', 'T', 'implementation')`, task, project)
	exec(`INSERT INTO jobs (id, task_id, project_id, phase, role, backend, status) VALUES ($1, $2, $3, 'implementation', 'worker', 'claude', 'running')`,
		job, task, project)
	exec(`INSERT INTO conversations (id, project_id, kind, name, visibility, created_by_kind) VALUES ($1, $2, 'channel', 'eng', 'project', 'system')`, channel, project)
	exec(`INSERT INTO messages (id, conversation_id, seq, author_kind, author_id, kind, body, origin, status)
		VALUES ($1, $2, 1, 'user', $3, 'text', 'hello', '{}', 'final')`, message, channel, user)

	tables := func() int {
		return count(`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('turn_checkpoints', 'review_comments', 'runtime_requests')`)
	}
	intent := func() int {
		return count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'messages' AND column_name = 'input_intent'`)
	}
	seed := func() {
		t.Helper()
		exec(`INSERT INTO turn_checkpoints (id, job_id, seq, repo, base_commit, head_commit, files, diff, diff_bytes)
			VALUES ('00000000-0000-0000-0000-0000000c0001', $1, 1, 'app', 'a', 'b', '[{"path":"x.go","added":1,"removed":0,"status":"added"}]', 'diff', 4)`, job)
		exec(`INSERT INTO review_comments (checkpoint_id, author_id, path, side, line_start, line_end, body, message_id)
			VALUES ('00000000-0000-0000-0000-0000000c0001', $1, 'x.go', 'new', 1, 2, 'rename it', $2)`, user, message)
		exec(`INSERT INTO runtime_requests (id, job_id, kind, tool, input, rule, expires_at)
			VALUES ('00000000-0000-0000-0000-0000000f0001', $1, 'tool_approval', 'Bash', '{"command":"curl x"}', 'Bash(curl:*)', now() + interval '30 minutes')`, job)
		exec(`UPDATE messages SET input_intent = 'queued' WHERE id = $1`, message)
		exec(`UPDATE jobs SET status = 'settling' WHERE id = $1`, job)
	}

	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	if tables() != 3 || intent() != 1 {
		t.Fatalf("up made %d tables, input_intent %d", tables(), intent())
	}
	seed()
	// What the constraints keep.
	for _, bad := range []string{
		`INSERT INTO review_comments (checkpoint_id, author_id, path, side, line_start, line_end, body)
			VALUES ('00000000-0000-0000-0000-0000000c0001', '` + user + `', 'x.go', 'middle', 1, 1, 'x')`,
		`INSERT INTO review_comments (checkpoint_id, author_id, path, side, line_start, line_end, body)
			VALUES ('00000000-0000-0000-0000-0000000c0001', '` + user + `', 'x.go', 'new', 5, 2, 'x')`,
		`INSERT INTO turn_checkpoints (job_id, seq, repo, base_commit, head_commit, files) VALUES ('` + job + `', 1, 'app', 'a', 'b', '[]')`,
		`INSERT INTO runtime_requests (id, job_id, kind, expires_at, status) VALUES (gen_random_uuid(), '` + job + `', 'tool_approval', now(), 'allowed')`,
		`UPDATE messages SET input_intent = 'later' WHERE id = '` + message + `'`,
		`UPDATE jobs SET status = 'settled' WHERE id = '` + job + `'`,
	} {
		if _, err := pool.Exec(ctx, bad); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	// A job's checkpoints, comments and requests go with it.
	if count(`SELECT count(*) FROM review_comments`) != 1 || count(`SELECT count(*) FROM runtime_requests`) != 1 {
		t.Fatal("seed")
	}

	if err := goose.DownToContext(ctx, conn, "migrations", before); err != nil {
		t.Fatal(err)
	}
	if tables() != 0 || intent() != 0 {
		t.Fatalf("down left %d tables, input_intent %d", tables(), intent())
	}
	if count(`SELECT count(*) FROM jobs WHERE id = $1 AND status = 'running'`, job) != 1 || count(`SELECT count(*) FROM messages`) != 1 ||
		count(`SELECT count(*) FROM tasks`) != 1 {
		t.Fatal("down took what it did not make, or left a job settling")
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'settling' WHERE id = $1`, job); err == nil {
		t.Fatal("down left settling a status")
	}

	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	seed()
	if tables() != 3 || count(`SELECT count(*) FROM jobs WHERE status = 'settling'`) != 1 {
		t.Fatal("second up")
	}
	exec(`DELETE FROM jobs WHERE id = $1`, job)
	if count(`SELECT count(*) FROM turn_checkpoints`)+count(`SELECT count(*) FROM review_comments`)+count(`SELECT count(*) FROM runtime_requests`) != 0 {
		t.Fatal("a job's checkpoints, comments and requests outlived it")
	}
}
