package process

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

func TestDefaultRoleGuides(t *testing.T) {
	for _, role := range []string{"researcher", "planner", "worker", "reviewer"} {
		g := DefaultRoleGuide(role)
		if !strings.Contains(g, "# Role: "+role) || !strings.Contains(g, "## ") {
			t.Errorf("%s guide missing or without output sections", role)
		}
	}
	if DefaultRoleGuide("astronaut") != "" {
		t.Error("unknown role should have no guide")
	}
	for role, want := range map[string]string{"researcher": "brief", "planner": "plan", "worker": "report", "reviewer": "review"} {
		if got := ArtifactTypeFor(role); got != want {
			t.Errorf("%s produces %s, want %s", role, got, want)
		}
	}
}

func setConfig(t *testing.T, s *Service, project pgtype.UUID, cfg string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), "UPDATE projects SET process_config = $2 WHERE id = $1", project, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestProjectRolesAndAutopilot(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	project, _ := newProject(t, pool)
	svc := service(t, pool)

	on, backend, err := svc.Autopilot(ctx, project, "claude")
	if err != nil || !on || backend != "claude" {
		t.Fatalf("default autopilot: %v %q %v", on, backend, err)
	}
	setConfig(t, svc, project, `{"roles":{"worker":"Custom worker rules"},"autopilot":{"enabled":false,"backend":"codex"}}`)
	on, backend, _ = svc.Autopilot(ctx, project, "claude")
	if on || backend != "codex" {
		t.Fatalf("configured autopilot: %v %q", on, backend)
	}
	if g, _ := svc.RoleGuide(ctx, project, "worker"); g != "Custom worker rules" {
		t.Fatalf("override: %q", g)
	}
	if g, _ := svc.RoleGuide(ctx, project, "planner"); !strings.Contains(g, "# Role: planner") {
		t.Fatal("roles without an override keep the default")
	}
}

func TestJobContextHasLatestArtifactsAndRollbackReason(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	project, user := newProject(t, pool)
	svc := service(t, pool)
	actor := Actor{Kind: ActorUser, ID: user}
	q := db.New(pool)

	task, err := svc.Create(ctx, CreateParams{ProjectID: project, Kind: "bug", Title: "crash", Description: "It crashes on login"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if task.Description != "It crashes on login" {
		t.Fatalf("description not stored: %q", task.Description)
	}
	v1, v2 := "plan v1", "plan v2"
	for _, c := range []*string{&v1, &v2} {
		if _, err := q.CreateArtifact(ctx, db.CreateArtifactParams{TaskID: task.ID, Phase: "planning", Type: "plan", Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Approve(ctx, task.ID, user); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Advance(ctx, task.ID, actor, ""); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("x", 30000)
	if _, err := q.CreateArtifact(ctx, db.CreateArtifactParams{TaskID: task.ID, Phase: "implementation", Type: "report", Content: &huge}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Rollback(ctx, task.ID, "planning", actor, "the plan missed the migration"); err != nil {
		t.Fatal(err)
	}

	docs, err := svc.JobContext(ctx, task.ID, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 || docs[0].Kind != "rollback" || docs[0].Body != "the plan missed the migration" {
		t.Fatalf("docs: %+v", docs)
	}
	var plan, report ContextDoc
	for _, d := range docs {
		switch d.Kind {
		case "plan":
			plan = d
		case "report":
			report = d
		}
	}
	if plan.Body != "plan v2" || !strings.Contains(plan.Title, "v2, approved") {
		t.Fatalf("only the latest, approved plan: %+v", plan)
	}
	if len(report.Body) > maxDocBytes+32 || !strings.HasSuffix(report.Body, "[truncated]") {
		t.Fatalf("report not truncated: %d bytes", len(report.Body))
	}
}

func TestInboxWaitsWhileAnAgentWorksAndFlagsFailedJobs(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	project, user := newProject(t, pool)
	svc := service(t, pool)
	q := db.New(pool)

	task, err := svc.Create(ctx, CreateParams{ProjectID: project, Kind: "bug", Title: "x"}, Actor{Kind: ActorUser, ID: user})
	if err != nil {
		t.Fatal(err)
	}
	reason := func() string {
		ds, err := svc.Inbox(ctx, InboxFilter{ProjectID: project})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			if d.Task.ID == task.ID {
				return string(d.Reason)
			}
		}
		return ""
	}
	if r := reason(); r != "approval" {
		t.Fatalf("no job yet: %q", r)
	}
	job, err := q.CreateJob(ctx, db.CreateJobParams{TaskID: task.ID, ProjectID: project, Phase: "planning", Role: "planner", Backend: "x", Bounds: []byte("{}"), MaxAttempts: 1, CreatedByKind: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if r := reason(); r != "" {
		t.Fatalf("while a job is queued the task should not ask for a decision: %q", r)
	}
	msg := "boom"
	if err := q.FinishJob(ctx, db.FinishJobParams{ID: job.ID, Status: "failed", Receipt: []byte("{}"), StopReason: &msg}); err != nil {
		t.Fatal(err)
	}
	if r := reason(); r != "job_failed" {
		t.Fatalf("a failed job needs a person: %q", r)
	}
}
