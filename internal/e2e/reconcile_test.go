package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
)

// A webhook is delivered at most once. GitHub dropped three in a row here to a 502 that never
// reached the server, and the gate went on showing a check that had already finished — the
// screen said "pending" for work that was done. Asking the plugin again is the only cure.
func TestAGateStuckOnPendingIsAskedAgain(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	var task gen.Task
	if code := h.do("POST", "/projects/"+p.id+"/tasks", gen.NewTask{Kind: "bug", Title: "Dropped webhook"}, &task); code != 201 {
		t.Fatalf("task: %d", code)
	}

	// Creating a task evaluates its gate, and that happens off the request. Wait for it, or
	// the check set below races with it and this test blames the wrong thing.
	waitForCheck(h, task.Id.String(), "echo-ci", gen.CheckStatusPass)

	// What a dropped "check finished" webhook leaves behind.
	if code := h.do("PUT", "/tasks/"+task.Id.String()+"/checks",
		gen.Check{Name: "echo-ci", Source: "plugin:echo", Status: gen.CheckStatusPending, Detail: ptr("waiting")}, nil); code != 200 {
		t.Fatalf("set pending check: %d", code)
	}
	if got := checkStatus(h, task.Id.String(), "echo-ci"); got != gen.CheckStatusPending {
		t.Fatalf("check should start pending, got %q", got)
	}

	// A gate that moved a moment ago is not stale: a check still running is left alone.
	// The count is not asserted: the development database is shared, so other tests' tasks
	// are in it too. What matters is this task.
	if _, err := h.plugins.ReconcileProjectChecks(context.Background(), projectUUID(p.id), time.Now().Add(-time.Hour), 50); err != nil {
		t.Fatal(err)
	}
	if got := checkStatus(h, task.Id.String(), "echo-ci"); got != gen.CheckStatusPending {
		t.Fatalf("a gate that just moved should be left alone, got %q", got)
	}

	// Age the gate the way an afternoon of waiting would.
	if _, err := h.pool.Exec(context.Background(),
		"UPDATE gates SET updated_at = now() - interval '1 hour' WHERE task_id = $1", task.Id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.plugins.ReconcileProjectChecks(context.Background(), projectUUID(p.id), time.Now().Add(-15*time.Minute), 50); err != nil {
		t.Fatal(err)
	}
	// echo answers "pass", which is what the lost webhook would have said.
	if got := checkStatus(h, task.Id.String(), "echo-ci"); got != gen.CheckStatusPass {
		t.Fatalf("check after reconciling = %q, want pass", got)
	}
}

// A closed task is nobody's business, however long its gate has sat.
func TestReconcilingLeavesClosedTasksAlone(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	var task gen.Task
	h.do("POST", "/projects/"+p.id+"/tasks", gen.NewTask{Kind: "chore", Title: "Already done"}, &task)
	waitForCheck(h, task.Id.String(), "echo-ci", gen.CheckStatusPass)
	h.do("PUT", "/tasks/"+task.Id.String()+"/checks",
		gen.Check{Name: "echo-ci", Source: "plugin:echo", Status: gen.CheckStatusPending}, nil)
	for _, sql := range []string{
		"UPDATE gates SET updated_at = now() - interval '1 hour' WHERE task_id = $1",
		"UPDATE tasks SET closed_at = now() WHERE id = $1",
	} {
		if _, err := h.pool.Exec(context.Background(), sql, task.Id.String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.plugins.ReconcileProjectChecks(context.Background(), projectUUID(p.id), time.Now().Add(-15*time.Minute), 50); err != nil {
		t.Fatal(err)
	}
	// Still pending: nobody asked the plugin about a task that is over.
	if got := checkStatus(h, task.Id.String(), "echo-ci"); got != gen.CheckStatusPending {
		t.Fatalf("a closed task was re-evaluated: its check became %q", got)
	}
}

// waitForCheck waits for the gate evaluation a task's creation sets off.
func waitForCheck(h *harness, taskID, name string, want gen.CheckStatus) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var d gen.TaskDetail
		h.do("GET", "/tasks/"+taskID, nil, &d)
		for _, c := range d.Gate.Checks {
			if c.Name == name && c.Status == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("check %q never became %q", name, want)
}

func checkStatus(h *harness, taskID, name string) gen.CheckStatus {
	h.t.Helper()
	var d gen.TaskDetail
	if code := h.do("GET", "/tasks/"+taskID, nil, &d); code != 200 {
		h.t.Fatalf("task detail: %d", code)
	}
	for _, c := range d.Gate.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	h.t.Fatalf("no check %q on the gate", name)
	return ""
}

// projectUUID parses the id the API handed back, so a test can scope work to its own project.
func projectUUID(id string) pgtype.UUID {
	var out pgtype.UUID
	_ = out.Scan(id)
	return out
}
