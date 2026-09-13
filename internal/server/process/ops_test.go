package process

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

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

func newProject(t *testing.T, pool *pgxpool.Pool) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	q := db.New(pool)
	p, err := q.CreateProject(ctx, db.CreateProjectParams{Slug: "t-" + randomSuffix(), Name: "test", Tags: []string{}, ProcessConfig: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	var uid pgtype.UUID
	err = pool.QueryRow(ctx, "INSERT INTO users(email,name) VALUES($1,'t') RETURNING id", "u-"+randomSuffix()+"@t.local").Scan(&uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM projects WHERE id=$1", p.ID) })
	return p.ID, uid
}

func randomSuffix() string {
	b := make([]byte, 6)
	for i := range b {
		b[i] = "abcdefghijklmnopqrstuvwxyz"[int(os.Getpid()+i*7919+int(testCounter))%26]
	}
	testCounter++
	return string(b) + itoa(testCounter)
}

var testCounter int

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var s []byte
	for n > 0 {
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	return string(s)
}

func service(t *testing.T, pool *pgxpool.Pool, caps ...string) *Service {
	t.Helper()
	cat, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return NewService(pool, StaticCatalog(cat), StaticCapabilities(caps))
}

func TestFeatureFlowEndsAtApprovedWithoutDeploy(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	projectID, userID := newProject(t, pool)
	svc := service(t, pool)
	user := Actor{Kind: ActorUser, ID: userID}

	task, err := svc.Create(ctx, CreateParams{ProjectID: projectID, Kind: "feature", Title: "Login"}, user)
	if err != nil {
		t.Fatal(err)
	}
	if task.Phase != "idea" {
		t.Fatalf("new feature starts in %q", task.Phase)
	}

	// idea: human gate, not yet approved → advance refused
	if _, err := svc.Advance(ctx, task.ID, user, ""); !errors.Is(err, ErrGateNotSatisfied) {
		t.Fatalf("advance without approval: %v", err)
	}
	for _, want := range []string{"discovery", "planning", "implementation"} {
		if err := svc.Approve(ctx, task.ID, userID); err != nil {
			t.Fatal(err)
		}
		task, err = svc.Advance(ctx, task.ID, user, "")
		if err != nil {
			t.Fatalf("advance to %s: %v", want, err)
		}
		if task.Phase != want {
			t.Fatalf("phase = %q, want %q", task.Phase, want)
		}
	}

	// implementation: gate=both → approval alone is not enough
	if err := svc.Approve(ctx, task.ID, userID); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetCheck(ctx, task.ID, Check{Name: "ci", Source: "plugin:github", Status: "fail", Detail: "tests red"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Advance(ctx, task.ID, user, ""); !errors.Is(err, ErrTaskBlocked) {
		t.Fatalf("advance with failing ci should be blocked: %v", err)
	}
	if err := svc.SetCheck(ctx, task.ID, Check{Name: "ci", Source: "plugin:github", Status: "pass"}); err != nil {
		t.Fatal(err)
	}
	task, err = svc.Advance(ctx, task.ID, user, "")
	if err != nil || task.Phase != "verification" {
		t.Fatalf("advance after ci pass: %q %v", task.Phase, err)
	}

	if err := svc.Approve(ctx, task.ID, userID); err != nil {
		t.Fatal(err)
	}
	task, err = svc.Advance(ctx, task.ID, user, "")
	if err != nil || task.Phase != "approved" {
		t.Fatalf("advance to approved: %q %v", task.Phase, err)
	}
	// approved is terminal without deploy: advancing closes
	task, err = svc.Advance(ctx, task.ID, user, "")
	if err != nil || !task.ClosedAt.Valid {
		t.Fatalf("closing at approved: closed=%v err=%v", task.ClosedAt.Valid, err)
	}
	if _, err := svc.Advance(ctx, task.ID, user, ""); !errors.Is(err, ErrTaskClosed) {
		t.Fatalf("advance on closed task: %v", err)
	}

	trs, err := db.New(pool).ListPhaseTransitions(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, tr := range trs {
		kinds[tr.Kind]++
	}
	if kinds["create"] != 1 || kinds["advance"] != 5 || kinds["close"] != 1 || kinds["auto_block"] != 1 || kinds["unblock"] != 1 {
		t.Fatalf("transition audit mismatch: %v", kinds)
	}
}

func TestRollbackRequiresReasonIsBackwardAndHitsCeiling(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	projectID, userID := newProject(t, pool)
	svc := service(t, pool)
	user := Actor{Kind: ActorUser, ID: userID}

	task, err := svc.Create(ctx, CreateParams{ProjectID: projectID, Kind: "bug", Title: "Crash"}, user)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Approve(ctx, task.ID, userID); err != nil {
		t.Fatal(err)
	}
	if task, err = svc.Advance(ctx, task.ID, user, ""); err != nil || task.Phase != "implementation" {
		t.Fatalf("to implementation: %q %v", task.Phase, err)
	}
	if _, err := svc.Rollback(ctx, task.ID, "planning", user, ""); err == nil {
		t.Fatal("rollback without reason must fail")
	}
	if _, err := svc.Rollback(ctx, task.ID, "verification", user, "x"); !errors.Is(err, ErrNotBackward) {
		t.Fatalf("forward rollback: %v", err)
	}

	// bug allows 2 rollbacks into a phase; the third blocks the task instead of moving it
	for i := 0; i < 2; i++ {
		task, err = svc.Rollback(ctx, task.ID, "planning", user, "plan was wrong")
		if err != nil || task.Phase != "planning" {
			t.Fatalf("rollback %d: %q %v", i, task.Phase, err)
		}
		if err := svc.Approve(ctx, task.ID, userID); err != nil {
			t.Fatal(err)
		}
		if task, err = svc.Advance(ctx, task.ID, user, ""); err != nil {
			t.Fatal(err)
		}
	}
	task, err = svc.Rollback(ctx, task.ID, "planning", user, "still wrong")
	if err != nil {
		t.Fatal(err)
	}
	if task.Phase != "implementation" || task.BlockedReason == nil {
		t.Fatalf("third rollback should block in place: phase=%q blocked=%v", task.Phase, task.BlockedReason)
	}
	if _, err := svc.Advance(ctx, task.ID, user, ""); !errors.Is(err, ErrTaskBlocked) {
		t.Fatalf("blocked task must not advance: %v", err)
	}
}

func TestChoreWithDeployHasRelease(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	projectID, userID := newProject(t, pool)
	svc := service(t, pool, "deploy")
	user := Actor{Kind: ActorUser, ID: userID}
	task, err := svc.Create(ctx, CreateParams{ProjectID: projectID, Kind: "chore", Title: "Bump deps"}, user)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Approve(ctx, task.ID, userID); err != nil {
		t.Fatal(err)
	}
	if task, err = svc.Advance(ctx, task.ID, user, ""); err != nil || task.Phase != "approved" {
		t.Fatalf("%q %v", task.Phase, err)
	}
	if task, err = svc.Advance(ctx, task.ID, user, ""); err != nil || task.Phase != "release" {
		t.Fatalf("with deploy, approved should lead to release: %q %v", task.Phase, err)
	}
}
