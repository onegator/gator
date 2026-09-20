package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

// A deploy is not finished the moment it lands. The observation window is the difference
// between "it shipped" and "it held", and nothing reported during it is the answer.
func TestAQuietObservationWindowFinishesTheTask(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	// Echo carries the deploy capability, so this project's chore template really has the
	// Release and Monitoring phases. Walk the task to Monitoring, which is where a release is
	// watched.
	// Release is the phase the deploy plugin owns; recording the release is what ends it.
	task := h.walkTo(p.id, gen.Chore, "Ship the thing", "release")

	body := map[string]any{"release": map[string]any{"version": "1.4.0", "task_id": task.Id.String(), "observe_minutes": 30}}
	if code, _ := h.hook(p.slug, "rel1", jsonBody(body), nil); code != 200 {
		t.Fatalf("release: %d", code)
	}

	var watching gen.Task
	h.do("GET", "/tasks/"+task.Id.String(), nil, &watching)
	if watching.Phase != "monitoring" {
		t.Fatalf("recording a release should finish the Release phase, got %s", watching.Phase)
	}

	// Before the window runs out, nothing happens: the release is still being watched.
	if n, err := h.plugins.SettleReleases(context.Background(), time.Now(), 50); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("a release still inside its window settled: %d", n)
	}

	if _, err := h.plugins.SettleReleases(context.Background(), time.Now().Add(31*time.Minute), 50); err != nil {
		t.Fatal(err)
	}
	var after gen.Task
	h.do("GET", "/tasks/"+task.Id.String(), nil, &after)
	if after.Phase == "monitoring" && after.ClosedAt == nil {
		t.Fatalf("a window that passed quietly should finish the task: %+v", after)
	}
}

// walkTo advances a new task until it reaches the wanted phase, approving on the way.
func (h *harness) walkTo(projectID string, kind gen.NewTaskKind, title, phase string) gen.Task {
	h.t.Helper()
	var task gen.Task
	if code := h.do("POST", "/projects/"+projectID+"/tasks", gen.NewTask{Kind: kind, Title: title}, &task); code != 201 {
		h.t.Fatalf("task: %d", code)
	}
	for range 8 {
		if task.Phase == phase {
			return task
		}
		h.do("POST", "/tasks/"+task.Id.String()+"/approve", nil, nil)
		if code := h.do("POST", "/tasks/"+task.Id.String()+"/advance", nil, &task); code != 200 {
			h.t.Fatalf("advance from %s: %d", task.Phase, code)
		}
	}
	h.t.Fatalf("never reached %s; stopped at %s", phase, task.Phase)
	return task
}

// One fault is one incident however often the monitoring tool repeats itself, and the first
// report is what opens a task for a person.
func TestAnIncidentBecomesOneTaskHoweverOftenItIsReported(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})

	report := func(delivery string) {
		body := map[string]any{"incident": map[string]any{
			"fingerprint": "checkout-500", "title": "Checkout answers 500", "severity": "critical"}}
		if code, _ := h.hook(p.slug, delivery, jsonBody(body), nil); code != 200 {
			t.Fatalf("incident %s: %d", delivery, code)
		}
	}
	report("i1")
	report("i2")
	report("i3")

	var tasks []gen.Task
	h.do("GET", "/projects/"+p.id+"/tasks", nil, &tasks)
	var incidents []gen.Task
	for _, item := range tasks {
		if item.Kind == "incident" {
			incidents = append(incidents, item)
		}
	}
	if len(incidents) != 1 {
		t.Fatalf("three reports of one fault should be one task, got %d", len(incidents))
	}
	// Severity decides how loudly it asks: critical goes to the top of the inbox.
	if incidents[0].Urgency != 1 {
		t.Errorf("a critical incident should be most urgent, got %d", incidents[0].Urgency)
	}
	if incidents[0].Title != "Checkout answers 500" {
		t.Errorf("title = %q", incidents[0].Title)
	}
}

// An open incident against a release is exactly the case the window exists to catch.
func TestAReleaseWithAnOpenIncidentIsNotCalledGood(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	task := h.walkTo(p.id, gen.Chore, "Ship something broken", "release")

	h.hook(p.slug, "rel2", jsonBody(map[string]any{
		"release": map[string]any{"version": "2.0.0", "task_id": task.Id.String(), "observe_minutes": 5}}), nil)
	h.hook(p.slug, "inc2", jsonBody(map[string]any{
		"incident": map[string]any{"fingerprint": "boom-2", "title": "It broke", "severity": "high", "version": "2.0.0"}}), nil)

	if _, err := h.plugins.SettleReleases(context.Background(), time.Now().Add(10*time.Minute), 50); err != nil {
		t.Fatal(err)
	}
	var after gen.Task
	h.do("GET", "/tasks/"+task.Id.String(), nil, &after)
	if after.ClosedAt != nil {
		t.Fatal("a release with an open incident should not close its task")
	}
}
