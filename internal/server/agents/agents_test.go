package agents

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

var serial atomic.Int64

func unique() string { return fmt.Sprintf("%d%d", time.Now().UnixNano(), serial.Add(1)) }

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("GATOR_DATABASE_URL")
	if url == "" {
		t.Skip("GATOR_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newProject(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(context.Background(), "INSERT INTO projects(slug,name) VALUES($1,'P') RETURNING id", "ag"+unique()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertAgent(ctx context.Context, pool *pgxpool.Pool, project pgtype.UUID, handle string) error {
	_, err := pool.Exec(ctx, `INSERT INTO agents (project_id, handle, display_name, source) VALUES ($1, $2, 'X', '{"kind":"custom"}')`, project, handle)
	return err
}

// Every project starts with one agent per role of the process, the ones Defaults names, and each
// of them has a prompt to work from.
func TestEveryProjectHasTheDefaultAgents(t *testing.T) {
	pool := testPool(t)
	project := newProject(t, pool)
	got, err := db.New(pool).ListAgentsVisibleIn(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	mine := map[string]db.Agent{}
	for _, a := range got {
		if a.ProjectID == project {
			mine[a.Handle] = a
		}
	}
	if len(mine) != len(Defaults) {
		t.Fatalf("a new project has %d agents of its own, want %d", len(mine), len(Defaults))
	}
	for _, d := range Defaults {
		a, ok := mine[d.Handle]
		if !ok {
			t.Fatalf("no @%s", d.Handle)
		}
		if a.RoleKey == nil || *a.RoleKey != d.RoleKey || a.Tier != d.Tier || a.DisplayName != d.DisplayName || a.Status != StatusActive {
			t.Fatalf("@%s is %+v, want %+v", d.Handle, a, d)
		}
		if a.Instructions != "" {
			t.Fatalf("a default agent speaks with its role's prompt, @%s has its own", d.Handle)
		}
		if process.DefaultRoleGuide(d.RoleKey) == "" {
			t.Fatalf("the role %q of @%s has no prompt", d.RoleKey, d.Handle)
		}
	}
}

// A handle seen in a project means one agent there: a project's own and the workspace's cannot
// share one, whichever comes first, and neither can two agents of one project.
func TestAHandleMeansOneAgentWhereItIsSeen(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	p, other := newProject(t, pool), newProject(t, pool)
	h := "h" + unique()
	none := pgtype.UUID{}

	cases := []struct {
		name          string
		first, second pgtype.UUID
		conflict      bool
	}{
		{"twice in one project", p, p, true},
		{"project first, then the workspace", p, none, true},
		{"the workspace first, then a project", none, p, true},
		{"twice in the workspace", none, none, true},
		{"two projects", p, other, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handle := fmt.Sprintf("%s-%d", h, i)
			if err := insertAgent(ctx, pool, c.first, handle); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM agents WHERE handle = $1", handle) })
			err := insertAgent(ctx, pool, c.second, handle)
			if c.conflict != (err != nil) {
				t.Fatalf("conflict %v, got %v", c.conflict, err)
			}
			if err != nil && !isUniqueViolation(err) {
				t.Fatalf("a taken handle is a unique violation, got %v", err)
			}
		})
	}

	// A built-in handle of every project is taken in the workspace too.
	if err := insertAgent(ctx, pool, none, "reviewer"); !isUniqueViolation(err) {
		t.Fatalf("a workspace @reviewer would hide every project's; got %v", err)
	}
	// Renaming into a taken handle is refused the same way.
	free := h + "-free"
	if err := insertAgent(ctx, pool, p, free); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM agents WHERE handle = $1", free) })
	if _, err := pool.Exec(ctx, "UPDATE agents SET handle = 'planner' WHERE handle = $1", free); !isUniqueViolation(err) {
		t.Fatalf("renaming to @planner should be refused, got %v", err)
	}
}

// Two people creating the same handle at the same moment, one in a project and one for the whole
// workspace, cannot both succeed.
func TestConcurrentHandlesTakeTurns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for round := range 10 {
		p := newProject(t, pool)
		handle := fmt.Sprintf("race-%s-%d", unique(), round)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, scope := range []pgtype.UUID{p, {}} {
			wg.Go(func() { errs[i] = insertAgent(ctx, pool, scope, handle) })
		}
		wg.Wait()
		_, _ = pool.Exec(ctx, "DELETE FROM agents WHERE handle = $1", handle)
		failed := 0
		for _, err := range errs {
			if err != nil {
				if !isUniqueViolation(err) {
					t.Fatal(err)
				}
				failed++
			}
		}
		if failed != 1 {
			t.Fatalf("round %d: exactly one of two racing creations must fail, %d did", round, failed)
		}
	}
}
