package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

// The dashboard counts, per plugin, what it brought in and is still open: a tracker's tasks,
// a monitoring plugin's incidents. A plugin switched on with nothing open still has its zero;
// one switched off has no entry; a stranger to the project hears nothing of it.
func TestDashboardCountsWhatEachPluginLeftOpen(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var project gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("dash%d", time.Now().UnixNano()), Name: "Dash"}, &project); code != 201 {
		t.Fatalf("project: %d", code)
	}
	pid := project.Id.String()
	n := time.Now().UnixNano()
	tracker, monitor, quiet, off := fmt.Sprintf("tracker%d", n), fmt.Sprintf("monitor%d", n), fmt.Sprintf("quiet%d", n), fmt.Sprintf("off%d", n)

	install := func(name, caps string, enabled bool) string {
		t.Helper()
		var id string
		if err := h.pool.QueryRow(ctx, `WITH p AS (INSERT INTO plugins (name, command, version, manifest)
			VALUES ($1, '{x}', '1', jsonb_build_object('capabilities', $2::jsonb)) RETURNING id)
			INSERT INTO project_plugins (project_id, plugin_id, config, secrets, enabled)
			SELECT $3, p.id, '{}', '\x', $4 FROM p RETURNING id::text`, name, caps, pid, enabled).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	install(tracker, `["tracker"]`, true)
	monitorID := install(monitor, `["monitoring"]`, true)
	install(quiet, `["monitoring"]`, true)
	install(off, `["tracker"]`, false)

	// Two open issues from the tracker and one it raised that is closed; one from the plugin
	// that is off, which no tile shows.
	raise := func(source string, closed bool) {
		t.Helper()
		var task gen.Task
		if code := h.do("POST", "/projects/"+pid+"/tasks", gen.NewTask{Kind: "bug", Title: "from " + source}, &task); code != 201 {
			t.Fatalf("task: %d", code)
		}
		if _, err := h.pool.Exec(ctx, `UPDATE tasks SET origin_source = $2, closed_at = CASE WHEN $3 THEN now() END WHERE id = $1`,
			task.Id.String(), source, closed); err != nil {
			t.Fatal(err)
		}
	}
	raise(tracker, false)
	raise(tracker, false)
	raise(tracker, true)
	raise(off, false)

	// Three open incidents from the monitor and one it closed.
	for i, closed := range []bool{false, false, false, true} {
		if _, err := h.pool.Exec(ctx, `INSERT INTO incidents (project_id, source_plugin_id, fingerprint, title, closed_at)
			VALUES ($1, $2, $3, 'boom', CASE WHEN $4 THEN now() END)`, pid, monitorID, fmt.Sprintf("f%d", i), closed); err != nil {
			t.Fatal(err)
		}
	}

	var got gen.DashboardCounts
	if code := h.do("GET", "/dashboard?projectId="+pid, nil, &got); code != 200 {
		t.Fatalf("dashboard: %d", code)
	}
	count := func(list []gen.PluginCount, plugin string) (int, bool) {
		for _, c := range list {
			if c.Plugin == plugin && c.ProjectId == project.Id {
				return c.Open, true
			}
		}
		return 0, false
	}
	if open, ok := count(got.Issues, tracker); !ok || open != 2 {
		t.Fatalf("the tracker's open issues: %d (listed %v) in %+v", open, ok, got)
	}
	if open, ok := count(got.Errors, monitor); !ok || open != 3 {
		t.Fatalf("the monitor's open errors: %d (listed %v) in %+v", open, ok, got)
	}
	if open, ok := count(got.Errors, quiet); !ok || open != 0 {
		t.Fatalf("a monitor with nothing open still says zero: %d (listed %v)", open, ok)
	}
	if _, ok := count(got.Issues, off); ok {
		t.Fatal("a plugin switched off has no number on the dashboard")
	}
	if _, ok := count(got.Issues, monitor); ok {
		t.Fatal("a monitoring plugin is no tracker")
	}

	// The whole workspace holds the same numbers for this project.
	var all gen.DashboardCounts
	if code := h.do("GET", "/dashboard", nil, &all); code != 200 {
		t.Fatalf("dashboard: %d", code)
	}
	if open, ok := count(all.Issues, tracker); !ok || open != 2 {
		t.Fatalf("across the workspace: %d (listed %v)", open, ok)
	}

	// A stranger sees none of it, and may not ask for the project by name.
	_, stranger := h.login("member")
	h.token = stranger
	var theirs gen.DashboardCounts
	if code := h.do("GET", "/dashboard", nil, &theirs); code != 200 {
		t.Fatalf("dashboard as a stranger: %d", code)
	}
	if _, ok := count(theirs.Issues, tracker); ok {
		t.Fatal("a stranger hears about a project they cannot read")
	}
	if _, ok := count(theirs.Errors, monitor); ok {
		t.Fatal("a stranger hears about a project they cannot read")
	}
	if code := h.do("GET", "/dashboard?projectId="+pid, nil, nil); code != 403 && code != 404 {
		t.Fatalf("a stranger asking for the project: %d", code)
	}
}
