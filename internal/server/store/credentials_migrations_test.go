package store

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

// 00055 (people's own model logins) goes up over a workspace that already ran jobs, down to
// where it started and up again. A workspace with jobs keeps running them on the runners' own
// logins; the logins, and what jobs and usage say of them, come and go whole.
func TestUserCredentialsMigrationRoundTrip(t *testing.T) {
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
	)
	exec(`INSERT INTO users (id, email, name) VALUES ($1, 'm@t.local', 'M')`, user)
	exec(`INSERT INTO projects (id, slug, name) VALUES ($1, 'alpha', 'Alpha')`, project)
	exec(`INSERT INTO tasks (id, project_id, kind, title, phase) VALUES ($1, $2, 'bug', 'Crash', 'triage')`, task, project)
	exec(`INSERT INTO jobs (id, task_id, project_id, phase, role, backend, instruction, created_by_kind)
		VALUES ($1, $2, $3, 'triage', 'triager', 'claude', '', 'system')`, job, task, project)
	exec(`INSERT INTO usage_records (task_id, project_id, phase, job_id, source, backend, model) VALUES ($1, $2, 'triage', $3, 'runner', 'claude', 'm')`,
		task, project, job)
	columns := func() int {
		return count(`SELECT count(*) FROM information_schema.columns WHERE
			(table_name = 'jobs' AND column_name IN ('payer_id', 'credential_id', 'credential_kind', 'credential_exclusive', 'waiting_reason')) OR
			(table_name = 'usage_records' AND column_name IN ('paid_by', 'credential_kind')) OR
			(table_name = 'workspace_settings' AND column_name = 'runner_logins')`)
	}
	tables := func() int {
		return count(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'user_credentials'`)
	}
	seed := func() {
		t.Helper()
		exec(`INSERT INTO user_credentials (user_id, kind, backend, secret, hint) VALUES ($1, 'claude_subscription', 'claude', 'v1.k.sealed', '…abcd')`, user)
		exec(`UPDATE jobs SET payer_id = $2, credential_id = (SELECT id FROM user_credentials), credential_kind = 'claude_subscription' WHERE id = $1`, job, user)
	}

	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	if tables() != 1 || columns() != 8 {
		t.Fatalf("up made %d tables and %d columns", tables(), columns())
	}
	if count(`SELECT count(*) FROM workspace_settings WHERE runner_logins`) != 1 {
		t.Fatal("a workspace that ran jobs stopped lending the runners' logins")
	}
	seed()
	for _, bad := range []string{
		`INSERT INTO user_credentials (user_id, kind, backend, secret) VALUES ('` + user + `', 'claude_subscription', 'claude', 'x')`,
		`INSERT INTO user_credentials (user_id, kind, backend, secret) VALUES ('` + user + `', 'Bad Kind', 'claude', 'x')`,
		`UPDATE user_credentials SET status = 'great'`,
	} {
		if _, err := pool.Exec(ctx, bad); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	if err := goose.DownToContext(ctx, conn, "migrations", before); err != nil {
		t.Fatal(err)
	}
	if tables() != 0 || columns() != 0 {
		t.Fatalf("down left %d tables and %d columns", tables(), columns())
	}
	if count(`SELECT count(*) FROM jobs`) != 1 || count(`SELECT count(*) FROM usage_records`) != 1 {
		t.Fatal("down took what it did not make")
	}
	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	seed()
	// Deleting the person takes their logins; the job's record keeps its payer.
	exec(`DELETE FROM users WHERE id = $1`, user)
	if count(`SELECT count(*) FROM user_credentials`) != 0 || count(`SELECT count(*) FROM jobs WHERE payer_id IS NOT NULL AND credential_id IS NULL`) != 1 {
		t.Fatal("a person's deletion")
	}
}
