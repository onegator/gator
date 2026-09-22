package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

var echo struct {
	once sync.Once
	path string
	err  error
}

// echoPlugin builds the test plugin once, to a stable path, so plugin rows left in a shared
// database by earlier runs still start.
func echoPlugin(t *testing.T) string {
	t.Helper()
	echo.once.Do(func() {
		echo.path = filepath.Join(os.TempDir(), "gator-e2e-echo-plugin")
		if out, err := exec.Command("go", "build", "-o", echo.path, "../server/plugins/testdata/echo").CombinedOutput(); err != nil {
			echo.err = fmt.Errorf("build echo: %v\n%s", err, out)
		}
	})
	if echo.err != nil {
		t.Fatal(echo.err)
	}
	return echo.path
}

type echoProject struct{ id, slug string }

// echoProject installs echo, creates a project and enables echo in it.
func (h *harness) echoProject(cfg map[string]any) echoProject {
	h.t.Helper()
	if code := h.do("PUT", "/plugins/echo", gen.InstallPlugin{Command: []string{echoPlugin(h.t)}}, nil); code != 200 {
		h.t.Fatalf("install: %d", code)
	}
	slug := fmt.Sprintf("pl%d", time.Now().UnixNano())
	var p gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: slug, Name: "Plugins"}, &p)
	h.t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), "UPDATE project_plugins SET enabled = false WHERE project_id = $1", p.Id.String())
	})
	if v, code := h.configureEcho(p.Id.String(), cfg); code != 200 {
		h.t.Fatalf("configure: %d %+v", code, v)
	}
	h.waitEcho(p.Id.String(), func(v gen.ProjectPlugin) bool { return v.Running })
	return echoProject{id: p.Id.String(), slug: slug}
}

func (h *harness) configureEcho(projectID string, cfg map[string]any) (gen.ProjectPlugin, int) {
	var v gen.ProjectPlugin
	code := h.do("PUT", "/projects/"+projectID+"/plugins/echo", gen.ProjectPluginSettings{Enabled: true, Config: &cfg}, &v)
	return v, code
}

func (h *harness) echoState(projectID string) gen.ProjectPlugin {
	var vs []gen.ProjectPlugin
	h.do("GET", "/projects/"+projectID+"/plugins", nil, &vs)
	for _, v := range vs {
		if v.Name == "echo" {
			return v
		}
	}
	return gen.ProjectPlugin{}
}

func (h *harness) waitEcho(projectID string, ok func(gen.ProjectPlugin) bool) gen.ProjectPlugin {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v := h.echoState(projectID)
		if ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("plugin state: %+v", v)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *harness) until(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *harness) hook(slug, delivery string, body []byte, hdr map[string]string) (int, map[string]string) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL+"/hooks/"+slug+"/echo", bytes.NewReader(body))
	if delivery != "" {
		req.Header.Set("X-Echo-Delivery", delivery)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func jsonBody(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func TestPluginSecretsStayInTheirProject(t *testing.T) {
	h := newHarness(t)
	a := h.echoProject(map[string]any{"greeting": "hi", "token": "tok-AAAA-1111"})
	b := h.echoProject(map[string]any{"greeting": "yo", "token": "tok-BBBB-2222"})

	v := h.echoState(a.id)
	if v.Config["token"] != nil || v.Config["greeting"] != "hi" || !slices.Equal(v.SecretsSet, []string{"token"}) || !slices.Contains(v.Capabilities, "ci") {
		t.Fatalf("a secret must never come back: %+v", v)
	}
	if _, code := h.configureEcho(a.id, map[string]any{"token": "x"}); code != 400 {
		t.Fatalf("missing required setting: %d", code)
	}
	if _, code := h.configureEcho(a.id, map[string]any{"greeting": "hi", "colour": "red"}); code != 400 {
		t.Fatalf("unknown setting: %d", code)
	}

	ui := func(p echoProject) string {
		var task gen.Task
		h.do("POST", "/projects/"+p.id+"/tasks", gen.NewTask{Kind: "chore", Title: "UI"}, &task)
		var out gen.PluginUI
		h.do("GET", "/tasks/"+task.Id.String()+"/plugin-ui", nil, &out)
		if len(out.Tabs) != 1 {
			return ""
		}
		return out.Tabs[0].Markdown
	}
	// Each project's process holds only that project's secret.
	if got := ui(a); got != "hi token=tok-AAAA-1111" {
		t.Fatalf("project A sees %q", got)
	}
	if got := ui(b); got != "yo token=tok-BBBB-2222" {
		t.Fatalf("project B sees %q", got)
	}
	// Changing a setting without resending the secret keeps the secret.
	if v, code := h.configureEcho(a.id, map[string]any{"greeting": "hello"}); code != 200 || !slices.Equal(v.SecretsSet, []string{"token"}) {
		t.Fatalf("reconfigure: %d %+v", code, v)
	}
	h.until("the restarted process", func() bool { return ui(a) == "hello token=tok-AAAA-1111" })

	// The audit has the calls, never the secret values.
	calls := h.count(`SELECT count(*) FROM plugin_calls c JOIN project_plugins pp ON pp.id = c.project_plugin_id WHERE pp.project_id IN ($1, $2)`, a.id, b.id)
	leaks := h.count(`SELECT count(*) FROM plugin_calls c JOIN project_plugins pp ON pp.id = c.project_plugin_id
		WHERE pp.project_id IN ($1, $2) AND concat(c.payload::text, c.result::text, c.error) LIKE '%tok-%'`, a.id, b.id)
	if calls == 0 || leaks != 0 {
		t.Fatalf("audit: %d calls, %d with a secret", calls, leaks)
	}
	var audit []gen.PluginCall
	if code := h.do("GET", "/projects/"+a.id+"/plugins/echo/calls?limit=5", nil, &audit); code != 200 || len(audit) == 0 {
		t.Fatalf("calls API: %d %d", code, len(audit))
	}
}

func TestPluginWebhookIsHandledOnce(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	issues := func() int {
		return h.count(`SELECT count(*) FROM tasks WHERE project_id = $1 AND external_refs->>'echo_id' = '42'`, p.id)
	}
	body := jsonBody(map[string]any{"id": "42", "title": "Crash on login"})
	if code, out := h.hook(p.slug, "d1", body, map[string]string{"Authorization": "Bearer hook-secret-xyz"}); code != 200 || out["status"] != "ok" {
		t.Fatalf("first delivery: %d %v", code, out)
	}
	if code, out := h.hook(p.slug, "d1", body, nil); code != 200 || out["status"] != "duplicate" {
		t.Fatalf("redelivery: %d %v", code, out)
	}
	if code, _ := h.hook(p.slug, "d2", body, nil); code != 200 {
		t.Fatalf("new delivery for the same issue: %d", code)
	}
	if n := issues(); n != 1 {
		t.Fatalf("one issue, one task: %d", n)
	}
	if code, _ := h.hook(p.slug, "d3", bytes.Repeat([]byte("a"), 1<<20+1024), nil); code != 413 {
		t.Fatalf("oversized body: %d", code)
	}
	if code, _ := h.hook(p.slug, "d4", jsonBody(map[string]any{"refuse": true}), nil); code != 400 {
		t.Fatalf("refused by the plugin: %d", code)
	}
	if code, _ := h.hook("no-such-project", "d5", body, nil); code != 404 {
		t.Fatalf("unknown hook: %d", code)
	}
	if n := h.count(`SELECT count(*) FROM plugin_calls c JOIN project_plugins pp ON pp.id = c.project_plugin_id
		WHERE pp.project_id = $1 AND c.payload::text LIKE '%hook-secret%'`, p.id); n != 0 {
		t.Fatal("the Authorization header must not reach the plugin")
	}
}

func TestPluginHooksReachGatesAndJobs(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	b := uniqueBackend()
	rec := &recordingExec{}
	h.startRunner(b, rec)
	var task gen.Task
	h.do("POST", "/projects/"+p.id+"/tasks", gen.NewTask{Kind: "bug", Title: "Login fails"}, &task)
	checks := func(phase string) string {
		var s string
		_ = h.pool.QueryRow(context.Background(), `SELECT checks::text FROM gates WHERE task_id = $1 AND phase = $2`, task.Id.String(), phase).Scan(&s)
		return s
	}
	h.until("gateEvaluate on the new task", func() bool { return strings.Contains(checks("planning"), "echo-ci") })
	if !strings.Contains(checks("planning"), `"plugin:echo"`) {
		t.Fatalf("check source: %s", checks("planning"))
	}

	j := h.job(task, b, "plan it", 0)
	h.waitJob(j.Id.String(), "done")
	prepared := false
	for _, d := range rec.last(t).Context {
		prepared = prepared || (d.Kind == "plugin" && strings.Contains(d.Body, "Echo says: hi"))
	}
	if !prepared {
		t.Fatalf("jobPrepare should add to the lease: %+v", rec.last(t).Context)
	}
	h.until("jobFinish leaves a note", func() bool {
		for _, a := range h.artifacts(task) {
			if a.Type == "echo-note" && a.Content != nil && strings.HasSuffix(*a.Content, " done") {
				return true
			}
		}
		return false
	})

	h.approveAdvance(task)
	h.until("phaseTransition", func() bool {
		var s string
		_ = h.pool.QueryRow(context.Background(), `SELECT kv.value #>> '{}' FROM plugin_kv kv JOIN project_plugins pp ON pp.id = kv.project_plugin_id
			WHERE pp.project_id = $1 AND kv.key = 'last_transition'`, p.id).Scan(&s)
		return s == "planning->implementation"
	})

	// A plugin cannot touch another project's task.
	other := h.task()
	if code, _ := h.hook(p.slug, "x1", jsonBody(map[string]any{"check_task": other.Id.String()}), nil); code != 502 {
		t.Fatalf("foreign task: %d", code)
	}
	if n := h.count(`SELECT count(*) FROM gates WHERE task_id = $1 AND checks::text LIKE '%echo-hook%'`, other.Id.String()); n != 0 {
		t.Fatal("a check landed on another project's task")
	}
}

func TestPluginTimeoutCrashAndBreaker(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})

	start := time.Now()
	if code, _ := h.hook(p.slug, "s1", jsonBody(map[string]any{"sleep_ms": 4000}), nil); code != 504 || time.Since(start) > 3500*time.Millisecond {
		t.Fatalf("slow plugin: %d after %s", code, time.Since(start))
	}
	if code, _ := h.hook(p.slug, "c1", jsonBody(map[string]any{"crash": true}), nil); code != 502 {
		t.Fatalf("crash: %d", code)
	}
	if code, _ := h.hook(p.slug, "ok1", jsonBody(map[string]any{"id": "7", "title": "after the crash"}), nil); code != 200 {
		t.Fatalf("the process restarts: %d", code)
	}

	for n := range 3 {
		if code, _ := h.hook(p.slug, fmt.Sprintf("f%d", n), jsonBody(map[string]any{"fail": true}), nil); code != 502 {
			t.Fatalf("failing call %d: %d", n, code)
		}
	}
	v := h.waitEcho(p.id, func(v gen.ProjectPlugin) bool { return !v.Enabled })
	if v.DisabledReason == nil || !strings.Contains(*v.DisabledReason, "failed calls in a row") {
		t.Fatalf("disabled: %+v", v)
	}
	if code, _ := h.hook(p.slug, "after", jsonBody(map[string]any{"id": "8"}), nil); code != 404 {
		t.Fatalf("a disabled plugin has no hook: %d", code)
	}
	// A switched-off plugin is something a person has to know about, so it is in the inbox —
	// not only on a settings page nobody opens until they wonder why nothing has happened.
	alert := h.pluginAlert(p.id, "The echo plugin was switched off")
	if alert == nil {
		t.Fatal("a plugin switched off by the breaker should raise an alert in the inbox")
	}
	// Enabling it again clears the reason, starts the process and closes the alert.
	h.configureEcho(p.id, map[string]any{"greeting": "hi"})
	h.waitEcho(p.id, func(v gen.ProjectPlugin) bool { return v.Enabled && v.Running && v.DisabledReason == nil })
	var after gen.Task
	h.do("GET", "/tasks/"+alert.Id.String(), nil, &after)
	if after.ClosedAt == nil {
		t.Fatal("switching the plugin back on should close its alert")
	}
}

// pluginAlert finds the inbox entry about a switched-off plugin, waiting for it to appear.
func (h *harness) pluginAlert(projectID, title string) *gen.Task {
	h.t.Helper()
	var found *gen.Task
	deadline := time.Now().Add(3 * time.Second)
	for found == nil && time.Now().Before(deadline) {
		for _, d := range h.decisions("?projectId=" + projectID) {
			// Checked as well as filtered: another test tripping the same plugin raises an
			// alert with the same title in its own project.
			if d.Task.Title == title && d.Task.ProjectId.String() == projectID {
				task := d.Task
				found = &task
			}
		}
		if found == nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
	return found
}

// The breaker must not depend on landing exactly on its threshold. Hooks run concurrently, so
// two failures can arrive at once and step over it; a plugin failing every call would then
// keep failing forever with nobody told.
func TestTheBreakerFiresEvenWhenFailuresArriveTogether(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})

	var wg sync.WaitGroup
	for n := range 6 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			h.hook(p.slug, fmt.Sprintf("burst%d", n), jsonBody(map[string]any{"fail": true}), nil)
		}(n)
	}
	wg.Wait()

	v := h.waitEcho(p.id, func(v gen.ProjectPlugin) bool { return !v.Enabled })
	if v.DisabledReason == nil || !strings.Contains(*v.DisabledReason, "failed calls in a row") {
		t.Fatalf("a burst of failures should disable the plugin with a reason: %+v", v)
	}
}
