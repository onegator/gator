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
	task := h.walkTo(p.id, gen.NewTaskKindChore, "Ship the thing", "release")

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
	task := h.walkTo(p.id, gen.NewTaskKindChore, "Ship something broken", "release")

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

// Fixing the incident is what lets the release be called good. The task closing is the whole
// signal: nobody should also have to remember to tick the incident off somewhere else.
func TestFixingTheIncidentLetsTheReleaseSettle(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	task := h.walkTo(p.id, gen.NewTaskKindChore, "Ship something shaky", "release")

	h.hook(p.slug, "rel3", jsonBody(map[string]any{
		"release": map[string]any{"version": "3.0.0", "task_id": task.Id.String(), "observe_minutes": 5}}), nil)
	h.hook(p.slug, "inc3", jsonBody(map[string]any{
		"incident": map[string]any{"fingerprint": "boom-3", "title": "It broke again", "severity": "high", "version": "3.0.0"}}), nil)

	var tasks []gen.Task
	h.do("GET", "/projects/"+p.id+"/tasks", nil, &tasks)
	var incident gen.Task
	for _, item := range tasks {
		if item.Kind == "incident" {
			incident = item
		}
	}
	if incident.Id.String() == "" {
		t.Fatal("the incident should have opened a task")
	}
	// Close the incident task the way a person would: approve and advance to the end.
	for range 10 {
		var current gen.Task
		if h.do("GET", "/tasks/"+incident.Id.String(), nil, &current); current.ClosedAt != nil {
			break
		}
		h.do("POST", "/tasks/"+incident.Id.String()+"/approve", nil, nil)
		if code := h.do("POST", "/tasks/"+incident.Id.String()+"/advance", nil, nil); code != 200 {
			break
		}
	}
	// The host hears task.closed on its own loop, so give it a moment to close the incident.
	h.until("the incident to be closed", func() bool {
		return h.count("SELECT count(*) FROM incidents WHERE project_id = $1 AND closed_at IS NOT NULL", p.id) == 1
	})

	if _, err := h.plugins.SettleReleases(context.Background(), time.Now().Add(10*time.Minute), 50); err != nil {
		t.Fatal(err)
	}
	var after gen.Task
	h.do("GET", "/tasks/"+task.Id.String(), nil, &after)
	if after.ClosedAt == nil && after.Phase == "monitoring" {
		t.Fatal("with the incident fixed, a quiet window should finish the release's task")
	}
}

// The screens read this: what is out there, and what is wrong with it.
func TestReleasesAndIncidentsAreReadable(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	h.hook(p.slug, "rel4", jsonBody(map[string]any{
		"release": map[string]any{"version": "4.1.0", "observe_minutes": 30}}), nil)
	h.hook(p.slug, "inc4", jsonBody(map[string]any{
		"incident": map[string]any{"fingerprint": "boom-4", "title": "Timeouts", "severity": "medium", "version": "4.1.0"}}), nil)

	var releases []gen.Release
	if code := h.do("GET", "/projects/"+p.id+"/releases", nil, &releases); code != 200 {
		t.Fatalf("releases: %d", code)
	}
	if len(releases) != 1 || releases[0].Version != "4.1.0" {
		t.Fatalf("releases = %+v", releases)
	}
	if !releases[0].Watching {
		t.Error("a release inside its window should read as still watched")
	}
	if releases[0].OpenIncidents == nil || *releases[0].OpenIncidents != 1 {
		t.Errorf("the open incident should be counted against the release: %+v", releases[0].OpenIncidents)
	}

	var incidents []gen.Incident
	if code := h.do("GET", "/projects/"+p.id+"/incidents", nil, &incidents); code != 200 {
		t.Fatalf("incidents: %d", code)
	}
	if len(incidents) != 1 || incidents[0].Title != "Timeouts" || incidents[0].ClosedAt != nil {
		t.Fatalf("incidents = %+v", incidents)
	}
}

// The two directions a monitoring plugin has to work in: the core asks it to deploy, and it
// tells the core the fault has stopped — after which the release can be called good again.
func TestTheCoreAsksTheDeployPluginAndHearsWhenTheFaultStops(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	task := h.walkTo(p.id, gen.NewTaskKindChore, "Ship and wobble", "release")

	// Entering Release is the ask, and the plugin records which task it was asked about.
	h.until("the deploy plugin to be asked", func() bool {
		return h.pluginKV(p.id, "asked_to_release") == task.Id.String()
	})

	h.hook(p.slug, "rel5", jsonBody(map[string]any{
		"release": map[string]any{"version": "5.0.0", "task_id": task.Id.String(), "observe_minutes": 5}}), nil)
	h.hook(p.slug, "inc5", jsonBody(map[string]any{
		"incident": map[string]any{"fingerprint": "boom-5", "title": "Latency", "severity": "high", "version": "5.0.0"}}), nil)
	if _, err := h.plugins.SettleReleases(context.Background(), time.Now().Add(10*time.Minute), 50); err != nil {
		t.Fatal(err)
	}
	if h.count("SELECT count(*) FROM releases WHERE project_id = $1 AND settled_at IS NOT NULL", p.id) != 0 {
		t.Fatal("a release with an open incident should not settle")
	}

	// The monitoring tool says it has stopped.
	h.hook(p.slug, "res5", jsonBody(map[string]any{"resolve": "boom-5"}), nil)
	if h.count("SELECT count(*) FROM incidents WHERE project_id = $1 AND closed_at IS NOT NULL", p.id) != 1 {
		t.Fatal("resolving should close the incident")
	}
	if _, err := h.plugins.SettleReleases(context.Background(), time.Now().Add(10*time.Minute), 50); err != nil {
		t.Fatal(err)
	}
	if h.count("SELECT count(*) FROM releases WHERE project_id = $1 AND settled_at IS NOT NULL", p.id) != 1 {
		t.Fatal("with nothing open against it, the release should settle")
	}
}

// pluginKV reads what the project's plugin remembered under key.
func (h *harness) pluginKV(projectID, key string) string {
	h.t.Helper()
	var s string
	_ = h.pool.QueryRow(context.Background(),
		`SELECT kv.value #>> '{}' FROM plugin_kv kv JOIN project_plugins pp ON pp.id = kv.project_plugin_id
		 WHERE pp.project_id = $1 AND kv.key = $2`, projectID, key).Scan(&s)
	return s
}
