package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// scratch is a database of its own, made for one test and dropped after it: migrations that go
// down cannot be tried on the database every other test shares.
func scratch(t *testing.T) (*sql.DB, *pgxpool.Pool) {
	t.Helper()
	base := os.Getenv("GATOR_DATABASE_URL")
	if base == "" {
		t.Skip("GATOR_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	name := fmt.Sprintf("gator_scratch_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	conn := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() {
		conn.Close()
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	return conn, pool
}

// The agents migrations go up over a database with work in it, down to where they started and up
// again, and both times every project gets its default agents and every old job its role's agent.
func TestAgentMigrationsRoundTrip(t *testing.T) {
	conn, pool := scratch(t)
	ctx := context.Background()
	const before = 38
	if err := goose.UpToContext(ctx, conn, "migrations", before); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO projects (id, slug, name) VALUES
		('00000000-0000-0000-0000-0000000000a1', 'alpha', 'Alpha'),
		('00000000-0000-0000-0000-0000000000b2', 'beta', 'Beta')`)
	exec(`INSERT INTO tasks (id, project_id, kind, title, phase) VALUES
		('00000000-0000-0000-0000-00000000aa01', '00000000-0000-0000-0000-0000000000a1', 'feature', 'A', 'planning'),
		('00000000-0000-0000-0000-00000000bb01', '00000000-0000-0000-0000-0000000000b2', 'bug', 'B', 'implementation')`)
	exec(`INSERT INTO jobs (id, task_id, project_id, phase, role, backend) VALUES
		('00000000-0000-0000-0000-0000000a0001', '00000000-0000-0000-0000-00000000aa01', '00000000-0000-0000-0000-0000000000a1', 'discovery', 'researcher', 'claude'),
		('00000000-0000-0000-0000-0000000a0002', '00000000-0000-0000-0000-00000000aa01', '00000000-0000-0000-0000-0000000000a1', 'planning', 'planner', 'claude'),
		('00000000-0000-0000-0000-0000000b0001', '00000000-0000-0000-0000-00000000bb01', '00000000-0000-0000-0000-0000000000b2', 'implementation', 'worker', 'claude'),
		('00000000-0000-0000-0000-0000000b0002', '00000000-0000-0000-0000-00000000bb01', '00000000-0000-0000-0000-0000000000b2', 'verification', 'security_reviewer', 'claude'),
		('00000000-0000-0000-0000-0000000b0003', '00000000-0000-0000-0000-00000000bb01', '00000000-0000-0000-0000-0000000000b2', 'verification', 'oddity', 'claude')`)

	check := func(round string) {
		t.Helper()
		var agents int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agents WHERE source->>'kind' = 'builtin'`).Scan(&agents); err != nil {
			t.Fatal(err)
		}
		if agents != 16 {
			t.Fatalf("%s: two projects should have eight default agents each, have %d", round, agents)
		}
		rows, err := pool.Query(ctx, `SELECT j.role, a.handle, a.project_id = j.project_id, j.tier, j.kind
			FROM jobs j LEFT JOIN agents a ON a.id = j.agent_id ORDER BY j.id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		want := map[string]string{"researcher": "researcher", "planner": "planner", "worker": "worker", "security_reviewer": "security", "oddity": ""}
		n := 0
		for rows.Next() {
			var role, tier, kind string
			var handle *string
			var same *bool
			if err := rows.Scan(&role, &handle, &same, &tier, &kind); err != nil {
				t.Fatal(err)
			}
			n++
			got := ""
			if handle != nil {
				got = *handle
				if !*same {
					t.Fatalf("%s: the %s job got another project's agent", round, role)
				}
			}
			if got != want[role] {
				t.Fatalf("%s: a %s job belongs to @%s, got @%s", round, role, want[role], got)
			}
			if tier != "work" || kind != "phase" {
				t.Fatalf("%s: an old job is a work-tier phase job, got %s/%s", round, tier, kind)
			}
		}
		if n != 5 {
			t.Fatalf("%s: %d jobs", round, n)
		}
	}

	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	check("first up")
	// A project made afterwards, by any route, has its agents from the start.
	exec(`INSERT INTO projects (id, slug, name) VALUES ('00000000-0000-0000-0000-0000000000c3', 'gamma', 'Gamma')`)
	var gamma int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agents WHERE project_id = '00000000-0000-0000-0000-0000000000c3'`).Scan(&gamma); err != nil {
		t.Fatal(err)
	}
	if gamma != 8 {
		t.Fatalf("a new project has %d agents", gamma)
	}
	exec(`DELETE FROM projects WHERE slug = 'gamma'`)

	if err := goose.DownToContext(ctx, conn, "migrations", before); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'jobs' AND column_name IN ('agent_id', 'turn_id', 'tier', 'kind')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_name IN ('agents', 'agent_turns', 'conversation_summaries')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if left != 0 || tables != 0 {
		t.Fatalf("down left %d columns and %d tables behind", left, tables)
	}

	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	check("second up")
}

// A default agent steps aside for a workspace agent that has its handle, rather than failing the
// creation of the project; projects made at the same moment do not wait on or fail each other.
func TestDefaultAgentsStepAsideAndProjectsDoNotCollide(t *testing.T) {
	conn, pool := scratch(t)
	ctx := context.Background()
	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agents (project_id, handle, display_name, source) VALUES (NULL, 'reviewer', 'Ours', '{"kind":"custom"}')`); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 8)
	for i := range 8 {
		go func() {
			_, err := pool.Exec(ctx, `INSERT INTO projects (slug, name) VALUES ($1, 'P')`, fmt.Sprintf("p%d", i))
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatalf("a project failed to be made: %v", err)
		}
	}
	var own, reviewers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE project_id IS NOT NULL), count(*) FILTER (WHERE handle = 'reviewer')
		FROM agents`).Scan(&own, &reviewers); err != nil {
		t.Fatal(err)
	}
	if own != 8*7 || reviewers != 1 {
		t.Fatalf("each project has the seven defaults the workspace left it: %d agents, %d @reviewer", own, reviewers)
	}
	// An agent put away frees its handle, and keeps its turns.
	if _, err := pool.Exec(ctx, `UPDATE agents SET deleted_at = now() WHERE project_id IS NULL AND handle = 'reviewer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT gator_seed_default_agents(id) FROM projects`); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agents WHERE handle = 'reviewer' AND deleted_at IS NULL`).Scan(&reviewers); err != nil || reviewers != 8 {
		t.Fatalf("every project takes @reviewer back: %d %v", reviewers, err)
	}
}
