package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// consentDecision is the full inbox's row asking to allow agents on the task, if there is one.
func (h *harness) consentDecision(projectID string, task gen.Task) *gen.Decision {
	h.t.Helper()
	d := has(h.decisions("?all=true&projectId="+projectID), task)
	if d == nil || d.Reason != gen.DecisionReasonAgentsNotAllowed {
		return nil
	}
	return d
}

// An agent works without asking for permission, so the autopilot waits until a person allows
// agents on the task, and the full inbox asks them to. Once allowed, it starts by itself.
func TestAutopilotWaitsForAPersonToAllowAgents(t *testing.T) {
	h := newHarness(t)
	task := h.task() // a bug in planning: a runner phase, autopilot on by default
	pid := task.ProjectId.String()
	if task.AgentsAllowedAt != nil {
		t.Fatal("a new task has nobody's consent")
	}
	if _, created := h.ensure(task); created {
		t.Fatal("autopilot must not start an agent nobody allowed")
	}

	d := h.consentDecision(pid, task)
	if d == nil {
		t.Fatalf("the full inbox should ask to allow agents: %+v", has(h.decisions("?all=true&projectId="+pid), task))
	}
	if d.Summary != "An agent is ready to start once you allow it." {
		t.Errorf("summary %q", d.Summary)
	}
	if len(d.Actions) == 0 || d.Actions[0] != gen.DecisionActionAllowAgents {
		t.Errorf("actions %v, want allow_agents first", d.Actions)
	}
	if d.Urgency != task.Urgency {
		t.Errorf("urgency %d, want the task's own %d, as for an approval", d.Urgency, task.Urgency)
	}
	// Older clients cannot read the reason, so the short inbox never carries it.
	if short := has(h.decisions("?projectId="+pid), task); short != nil && short.Reason == gen.DecisionReasonAgentsNotAllowed {
		t.Fatal("agents_not_allowed is only for all=true")
	}

	ctx := t.Context()
	go h.mgr.RunAutopilot(ctx)
	time.Sleep(50 * time.Millisecond)
	allowed := h.allowAgents(task)
	if allowed.AgentsAllowedAt == nil || allowed.AgentsAllowedBy == nil {
		t.Fatalf("consent should be recorded with who gave it: %+v %+v", allowed.AgentsAllowedAt, allowed.AgentsAllowedBy)
	}
	// The harness has no relay, so the event the service wrote reaches the autopilot by hand.
	h.hub.Publish(events.Event{Type: "task.agents_allowed", Aggregate: "task", AggregateID: task.Id.String(), Payload: []byte(`{}`)})
	h.waitFor(t, "the autopilot's job", func() bool {
		var jobs []gen.Job
		h.do("GET", "/tasks/"+task.Id.String()+"/jobs", nil, &jobs)
		return len(jobs) == 1
	})
	if h.consentDecision(pid, task) != nil {
		t.Fatal("an allowed task no longer asks")
	}
}

// Allowing twice keeps the first answer and tells the history once.
func TestAllowingAgentsTwiceChangesNothing(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	first := h.allowAgents(task)
	time.Sleep(10 * time.Millisecond)
	second := h.allowAgents(task)
	if first.AgentsAllowedAt == nil || second.AgentsAllowedAt == nil || !first.AgentsAllowedAt.Equal(*second.AgentsAllowedAt) {
		t.Fatalf("the first moment should stay: %v then %v", first.AgentsAllowedAt, second.AgentsAllowedAt)
	}
	var trs []gen.Transition
	h.do("GET", "/tasks/"+task.Id.String()+"/transitions", nil, &trs)
	n := 0
	for _, tr := range trs {
		if deref(tr.Reason) == process.AllowedAgentsReason {
			n++
			if tr.ActorKind != "user" || tr.ActorId == nil {
				t.Errorf("the history should say which person allowed it: %+v", tr)
			}
		}
	}
	if n != 1 {
		t.Fatalf("history tells of the consent %d times, want once", n)
	}
	// The detail carries it too.
	var detail gen.TaskDetail
	h.do("GET", "/tasks/"+task.Id.String(), nil, &detail)
	if detail.AgentsAllowedAt == nil || detail.AgentsAllowedBy == nil {
		t.Fatal("the task detail should say agents are allowed, and by whom")
	}
}

// A job started by hand needs the same consent: refused without it, and given in the same step
// with allowAgents.
func TestManualJobNeedsConsent(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	path := "/tasks/" + task.Id.String() + "/jobs"
	var refusal gen.Error
	if code := h.do("POST", path, gen.NewJob{Backend: &backend}, &refusal); code != 409 {
		t.Fatalf("a job nobody allowed: %d, want 409", code)
	}
	if deref(refusal.Code) != "agents_not_allowed" || !strings.Contains(refusal.Error, "allow") {
		t.Fatalf("the refusal should say what is missing: %+v", refusal)
	}
	var jobs []gen.Job
	h.do("GET", path, nil, &jobs)
	if len(jobs) != 0 {
		t.Fatal("a refused job must not be queued")
	}

	var job gen.Job
	if code := h.do("POST", path, gen.NewJob{Backend: &backend, AllowAgents: ptr(true)}, &job); code != 201 {
		t.Fatalf("allowed and queued in one go: %d", code)
	}
	var after gen.TaskDetail
	h.do("GET", "/tasks/"+task.Id.String(), nil, &after)
	if after.AgentsAllowedAt == nil {
		t.Fatal("allowAgents should record the consent")
	}
	// From then on a job needs nothing more.
	if code := h.do("POST", path, gen.NewJob{Backend: &backend}, nil); code != 201 {
		t.Fatalf("a second job on an allowed task: %d", code)
	}
}

// Outside work is let in before anything else is asked; admitting it does not allow agents.
func TestAdmittingOutsideWorkDoesNotAllowAgents(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	if code, _ := h.hook(p.slug, "consent1", jsonBody(map[string]any{"id": "91", "title": "Outside asks for a README"}), nil); code != 200 {
		t.Fatalf("webhook: %d", code)
	}
	var outside gen.Task
	h.until("the plugin's task", func() bool {
		var tasks []gen.Task
		h.do("GET", "/projects/"+p.id+"/tasks", nil, &tasks)
		for _, task := range tasks {
			if task.Title == "Outside asks for a README" {
				outside = task
				return true
			}
		}
		return false
	})
	if d := has(h.decisions("?all=true&projectId="+p.id), outside); d == nil || d.Reason != gen.DecisionReasonExternal {
		t.Fatalf("outside work is let in first: %+v", d)
	}
	if code := h.do("POST", "/tasks/"+outside.Id.String()+"/admit", nil, nil); code != 200 {
		t.Fatalf("admit: %d", code)
	}
	if h.consentDecision(p.id, outside) == nil {
		t.Fatal("once admitted, the inbox should ask to allow agents")
	}
	if _, created := h.ensure(outside); created {
		t.Fatal("admitting must not start an agent nobody allowed")
	}
	h.allowAgents(outside)
	if _, created := h.ensure(outside); !created {
		t.Fatal("admitted and allowed, the autopilot starts")
	}
}

// Work that already had a job when consent was introduced counts as allowed, from its first
// job, so nothing running stops. This plays the migration's own statement in a transaction
// that is rolled back, so the shared database keeps what other tests rely on.
func TestTasksWithJobsCountAsAllowed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	worked, idle := h.task(), h.task()
	q := db.New(h.pool)
	id := pgtype.UUID{Bytes: worked.Id, Valid: true}
	first := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Microsecond)
	for i, at := range []time.Time{first.Add(time.Hour), first} {
		j, err := q.CreateJob(ctx, db.CreateJobParams{TaskID: id, ProjectID: pgtype.UUID{Bytes: worked.ProjectId, Valid: true},
			Phase: worked.Phase, Role: "planner", Backend: "x", Bounds: []byte("{}"), MaxAttempts: 1, CreatedByKind: "system"})
		if err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
		if _, err := h.pool.Exec(ctx, "UPDATE jobs SET created_at = $2, status = 'done' WHERE id = $1", j.ID, at); err != nil {
			t.Fatal(err)
		}
	}

	raw, err := os.ReadFile("../server/store/migrations/00033_agent_consent.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := strings.SplitN(string(raw), "-- +goose Down", 2)[0]
	start := strings.Index(up, "UPDATE tasks")
	if start < 0 {
		t.Fatal("the migration has no backfill")
	}
	backfill := up[start : start+strings.Index(up[start:], ";")+1]

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, backfill); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var at pgtype.Timestamptz
	var by pgtype.UUID
	if err := tx.QueryRow(ctx, "SELECT agents_allowed_at, agents_allowed_by FROM tasks WHERE id = $1", id).Scan(&at, &by); err != nil {
		t.Fatal(err)
	}
	if !at.Valid || !at.Time.Equal(first) {
		t.Fatalf("a task with jobs is allowed from its first job (%v), got %v", first, at)
	}
	if by.Valid {
		t.Error("nobody was asked, so nobody is named")
	}
	if err := tx.QueryRow(ctx, "SELECT agents_allowed_at FROM tasks WHERE id = $1", idle.Id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at.Valid {
		t.Fatal("a task without jobs still waits for a person")
	}
}
