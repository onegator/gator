package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, e *Env, args ...string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Run(ctx, e, args)
}

// browser follows the sign-in like a person's browser would: every redirect, ending at the
// CLI's loopback listener.
func browser(t *testing.T, before func(loginURL string)) func(string) error {
	return func(loginURL string) error {
		go func() {
			if before != nil {
				before(loginURL)
			}
			res, err := http.Get(loginURL)
			if err != nil {
				t.Errorf("browser: %v", err)
				return
			}
			res.Body.Close()
		}()
		return nil
	}
}

func TestLoginThroughBrowser(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	var loopback string
	e.OpenBrowser = browser(t, func(loginURL string) {
		u, _ := url.Parse(loginURL)
		loopback = u.Query().Get("redirect_uri")
		// Someone else's request to the port is turned away and the real one still lands.
		res, err := http.Get(loopback + "?code=stolen&state=not-ours")
		if err != nil {
			t.Errorf("probe: %v", err)
			return
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("a callback with the wrong state answered %d, want 400", res.StatusCode)
		}
	})

	if code := run(t, e, "login", "--server", f.srv.URL+"/"); code != 0 {
		t.Fatalf("login exited %d: %s", code, errOut)
	}
	if !strings.HasPrefix(loopback, "http://127.0.0.1:") || !strings.HasSuffix(loopback, "/callback") {
		t.Errorf("redirect_uri = %q, want a loopback callback", loopback)
	}
	if !strings.Contains(errOut.String(), f.srv.URL+"/auth/login?") {
		t.Errorf("the sign-in address is not printed for a browser that does not open:\n%s", errOut)
	}
	want := "Signed in to " + f.srv.URL + " as Ada Lovelace.\n"
	if !strings.HasPrefix(out.String(), want) {
		t.Errorf("stdout = %q, want it to start with %q", out, want)
	}
	if tok, err := e.Store.Get(f.srv.URL); err != nil || tok != f.token {
		t.Errorf("stored token = %q, %v; want %q", tok, err, f.token)
	}
	cfg, _ := LoadConfig(e.ConfigDir)
	if cfg.Server != f.srv.URL {
		t.Errorf("config server = %q, want %q (normalized)", cfg.Server, f.srv.URL)
	}
}

func TestLoginRefusedByProvider(t *testing.T) {
	f := newFake(t)
	f.loginError = "access_denied"
	e, _, errOut := f.env(t)
	e.OpenBrowser = browser(t, nil)
	if code := run(t, e, "login", "--server", f.srv.URL); code != exitAuth {
		t.Fatalf("login exited %d, want %d: %s", code, exitAuth, errOut)
	}
	if !strings.Contains(errOut.String(), "access_denied") {
		t.Errorf("stderr does not say why: %s", errOut)
	}
	if _, err := e.Store.Get(f.srv.URL); err != ErrNoToken {
		t.Errorf("a refused sign-in stored something: %v", err)
	}
}

func TestLoginWithoutOIDCAsksForToken(t *testing.T) {
	f := newFake(t)
	f.noOIDC = true
	e, _, errOut := f.env(t)
	if code := run(t, e, "login", "--server", f.srv.URL); code != exitUserErr {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	if !strings.Contains(errOut.String(), "--token") {
		t.Errorf("stderr does not point at --token: %s", errOut)
	}
}

func TestLoginWithPastedToken(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	e.Stdin = strings.NewReader(f.token + "\n")
	if code := run(t, e, "login", f.srv.URL, "--token", "-"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	if !strings.Contains(out.String(), "as Ada Lovelace") {
		t.Errorf("stdout = %q", out)
	}

	e2, _, errOut2 := f.env(t)
	if code := run(t, e2, "login", "--server", f.srv.URL, "--token", "gtr_user_wrong"); code == 0 {
		t.Fatal("a token the server refuses was accepted")
	}
	if _, err := e2.Store.Get(f.srv.URL); err != ErrNoToken {
		t.Errorf("a refused token was stored: %v (%s)", err, errOut2)
	}
}

func TestLogoutRevokesAndForgets(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	f.signIn(t, e)
	if code := run(t, e, "logout"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	if !f.revoked {
		t.Error("the token was not revoked on the server")
	}
	if _, err := e.Store.Get(f.srv.URL); err != ErrNoToken {
		t.Errorf("the token is still stored: %v", err)
	}
	if out.String() != "Signed out of "+f.srv.URL+".\n" {
		t.Errorf("stdout = %q", out)
	}
	if code := run(t, e, "whoami"); code != exitAuth {
		t.Errorf("whoami after logout exited %d, want %d", code, exitAuth)
	}
}

func TestWhoamiAndEnvironmentOverrides(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	vars := map[string]string{"GATOR_SERVER": f.srv.URL, "GATOR_TOKEN": f.token}
	e.Getenv = func(k string) string { return vars[k] }
	if code := run(t, e, "whoami"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	if want := "Ada Lovelace (user, workspace member) on " + f.srv.URL + "\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestStoredSignInIsNotLentToAJob(t *testing.T) {
	f := newFake(t)
	e, _, errOut := f.env(t)
	f.signIn(t, e)
	vars := map[string]string{"GATOR_JOB_TOKEN": "gtr_job_x"}
	e.Getenv = func(k string) string { return vars[k] }
	if code := run(t, e, "projects"); code != exitAuth {
		t.Fatalf("exited %d, want %d", code, exitAuth)
	}
	if !strings.Contains(errOut.String(), "gator-cli") {
		t.Errorf("stderr does not point the agent at gator-cli: %s", errOut)
	}
}

func TestProjectsTable(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	f.signIn(t, e)
	if code := run(t, e, "use", "shop"); code != 0 {
		t.Fatalf("use exited %d: %s", code, errOut)
	}
	out.Reset()
	if code := run(t, e, "projects"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	want := "" +
		"   SLUG  NAME      ROLE\n" +
		"*  shop  Shop      admin\n" +
		"   old   Old site  viewer (archived)\n"
	if out.String() != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
}

func TestProjectCreateBecomesCurrent(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	f.signIn(t, e)
	if code := run(t, e, "project", "create", "blog", "--repo", "acme/blog", "--branch", "trunk"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	want := "Created project blog (blog); you are its admin.\nIt is now the current project; gator tasks and gator task new use it.\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	cfg, _ := LoadConfig(e.ConfigDir)
	if cfg.Project != "blog" {
		t.Errorf("current project = %q", cfg.Project)
	}
	if code := run(t, e, "project", "create", "blog"); code != exitConflict {
		t.Errorf("a taken slug exited %d, want %d", code, exitConflict)
	}
}

func TestTasksTableAndFilters(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	f.signIn(t, e)
	if code := run(t, e, "tasks", "--project", "shop"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	want := "" +
		"ID                                    KIND     PHASE             TITLE\n" +
		"22222222-2222-4222-8222-222222222222  feature  implement         Checkout with Apple Pay\n" +
		"66666666-6666-4666-8666-666666666666  bug      triage (blocked)  Cart forgets items\n"
	if out.String() != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}

	// Without a current project every (unarchived) project is listed, with its slug.
	out.Reset()
	if code := run(t, e, "tasks", "--mine", "--json"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	var rows []struct {
		ID          string `json:"id"`
		ProjectSlug string `json:"projectSlug"`
	}
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("--json is not JSON: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0].ID != taskID || rows[0].ProjectSlug != "shop" {
		t.Errorf("--mine = %+v, want only the task handed to the caller", rows)
	}
}

func TestTaskShow(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	f.signIn(t, e)
	if code := run(t, e, "task", "show", taskID); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	want := "" +
		"Checkout with Apple Pay\n" +
		"feature · " + taskID + " · urgency 2\n" +
		"Phase: idea → plan → [implement] → review → done\n" +
		"\nGate (both): waits for a person's approval (gator task approve)\n" +
		"  pass    tests\n" +
		"  pending review — waiting\n"
	if out.String() != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
	if code := run(t, e, "task", "show", "00000000-0000-4000-8000-000000000000"); code != exitUserErr {
		t.Errorf("an unknown task exited %d, want %d", code, exitUserErr)
	}
}

func TestTaskNewUsesCurrentProject(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	f.signIn(t, e)
	if code := run(t, e, "task", "new", "Add", "dark", "mode", "--kind", "chore"); code != exitUserErr {
		t.Fatalf("without a project exited %d, want %d", code, exitUserErr)
	}
	if code := run(t, e, "use", "shop"); code != 0 {
		t.Fatal(errOut)
	}
	out.Reset()
	if code := run(t, e, "task", "new", "--kind", "bug", "Cart forgets items again", "--description", "since Monday"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	if !strings.HasPrefix(out.String(), "Created bug 99999999-9999-4999-8999-999999999999 in shop, phase idea.\n") {
		t.Errorf("stdout = %q", out)
	}
	if code := run(t, e, "task", "new", "--kind", "epic", "x"); code != exitUserErr {
		t.Errorf("an unknown kind exited %d", code)
	}
}

func TestTaskAdvanceAndApprove(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	f.signIn(t, e)
	if code := run(t, e, "task", "advance", taskID, "--reason", "looks done"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	if out.String() != "Task "+taskID+" is now in review.\n" {
		t.Errorf("stdout = %q", out)
	}
	errOut.Reset()
	if code := run(t, e, "task", "approve", taskID); code != exitConflict {
		t.Errorf("a refused approval exited %d, want %d", code, exitConflict)
	}
	if !strings.Contains(errOut.String(), "gate not satisfied") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestTaskRunCloudSendsPlacement(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	f.signIn(t, e)
	if code := run(t, e, "task", "run", taskID, "--cloud"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	if code := run(t, e, "task", "run", taskID); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	if len(f.newJobs) != 2 || f.newJobs[0]["placement"] != "cloud" {
		t.Fatalf("jobs asked for = %v, want placement cloud first", f.newJobs)
	}
	if _, ok := f.newJobs[1]["placement"]; ok {
		t.Errorf("a local run sent placement %v; an older server would refuse it", f.newJobs[1]["placement"])
	}
	if !strings.HasPrefix(out.String(), "Queued job "+jobID+" for implement (developer, cloud).\n") {
		t.Errorf("stdout = %q", out)
	}
}

func TestTaskRunCloudOnAnOlderServer(t *testing.T) {
	f := newFake(t)
	f.rejectPlacement = true
	e, _, errOut := f.env(t)
	f.signIn(t, e)
	if code := run(t, e, "task", "run", taskID, "--cloud"); code != exitUserErr {
		t.Fatalf("exited %d, want %d", code, exitUserErr)
	}
	if !strings.Contains(errOut.String(), "does not know about cloud runs yet") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestTaskLogsFollowsOverWebSocket(t *testing.T) {
	f := newFake(t)
	e, out, errOut := f.env(t)
	f.signIn(t, e)
	f.jobs[taskID] = []Job{{ID: jobID, TaskID: taskID, ProjectID: projID, Phase: "implement", Role: "developer", Backend: "claude", Status: "running", CreatedAt: t0}}
	f.addEvents(jobID,
		JobEvent{Seq: 1, Type: "session", At: t0, Payload: map[string]any{"model": "opus"}},
		JobEvent{Seq: 2, Type: "text", At: t0, Payload: map[string]any{"text": "Reading the checkout code."}},
	)
	f.onSubscribe = func(send func(string, any)) {
		// Seq 2 arrives twice (backlog and live); seq 4 is stored but its live message lost.
		send("job.output", JobEvent{Seq: 2, Type: "text", At: t0, Payload: map[string]any{"text": "Reading the checkout code."}})
		send("job.output", JobEvent{Seq: 3, Type: "tool_call", At: t0, Payload: map[string]any{"name": "Bash", "input": map[string]any{"command": "go test ./...\nmore"}}})
		f.addEvents(jobID, JobEvent{Seq: 3, Type: "tool_call", At: t0, Payload: map[string]any{"name": "Bash", "input": map[string]any{"command": "go test ./...\nmore"}}},
			JobEvent{Seq: 4, Type: "tool_result", At: t0, Payload: map[string]any{"is_error": true, "content": "FAIL checkout"}},
			JobEvent{Seq: 5, Type: "result", At: t0, Payload: map[string]any{"subtype": "success", "num_turns": 7.0, "cost_usd": 0.4312}})
		send("job.output", JobEvent{Seq: 5, Type: "result", At: t0, Payload: map[string]any{"subtype": "success", "num_turns": 7.0, "cost_usd": 0.4312}})
		f.mu.Lock()
		f.finishJob(jobID, "done")
		f.mu.Unlock()
		send("job.finished", map[string]any{"status": "done"})
	}
	if code := run(t, e, "task", "logs", taskID, "-f"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	want := "" +
		"· session started (opus)\n" +
		"Reading the checkout code.\n" +
		"→ Bash go test ./... …\n" +
		"  ✗ FAIL checkout\n" +
		"— success, 7 turns, $0.43\n"
	if out.String() != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
	if strings.Contains(errOut.String(), "no live stream") {
		t.Errorf("fell back to polling: %s", errOut)
	}
	if !strings.Contains(errOut.String(), "Job "+jobID+" done") {
		t.Errorf("stderr does not say how the job ended: %s", errOut)
	}
}

func TestTaskLogsFallsBackToPolling(t *testing.T) {
	f := newFake(t)
	f.noWebSocket = true
	e, out, errOut := f.env(t)
	f.signIn(t, e)
	f.jobs[taskID] = []Job{{ID: jobID, TaskID: taskID, ProjectID: projID, Phase: "implement", Role: "developer", Backend: "claude", Status: "running", CreatedAt: t0}}
	f.addEvents(jobID, JobEvent{Seq: 1, Type: "text", At: t0, Payload: map[string]any{"text": "one"}})
	f.pollsUntilDone = 3
	go func() {
		time.Sleep(15 * time.Millisecond)
		f.addEvents(jobID, JobEvent{Seq: 2, Type: "text", At: t0, Payload: map[string]any{"text": "two"}})
	}()
	if code := run(t, e, "task", "logs", taskID, "-f", "--json"); code != 0 {
		t.Fatalf("exited %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one JSON line per event, got:\n%s", out)
	}
	var ev JobEvent
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil || ev.Seq != 2 || ev.Payload["text"] != "two" {
		t.Errorf("second line = %s (%v)", lines[1], err)
	}
	if !strings.Contains(errOut.String(), "no live stream") {
		t.Errorf("stderr does not say it polls: %s", errOut)
	}
}

func TestTaskLogsWithoutJobs(t *testing.T) {
	f := newFake(t)
	e, _, errOut := f.env(t)
	f.signIn(t, e)
	if code := run(t, e, "task", "logs", taskID); code != exitUserErr {
		t.Fatalf("exited %d", code)
	}
	if !strings.Contains(errOut.String(), "gator task run "+taskID) {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestConfigRoundTripAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gator")
	if c, err := LoadConfig(dir); err != nil || c != (Config{}) {
		t.Fatalf("missing config = %+v, %v; want empty", c, err)
	}
	if err := SaveConfig(dir, Config{Server: "https://g.example", Project: "shop"}); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(dir)
	if err != nil || c.Server != "https://g.example" || c.Project != "shop" {
		t.Fatalf("round trip = %+v, %v", c, err)
	}
	store := FileStore{Path: filepath.Join(dir, "credentials.json")}
	if err := store.Set("https://g.example", "gtr_user_a"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{configPath(dir), store.Path} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, %v; want 0600", p, st.Mode().Perm(), err)
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", st.Mode().Perm())
	}
	if err := store.Delete("https://g.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("https://g.example"); err != ErrNoToken {
		t.Errorf("after delete: %v", err)
	}
}

func TestConfigDir(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := ConfigDir(env(map[string]string{"HOME": "/home/ada"})); got != "/home/ada/.config/gator" {
		t.Errorf("got %s", got)
	}
	if got := ConfigDir(env(map[string]string{"HOME": "/home/ada", "XDG_CONFIG_HOME": "/cfg"})); got != "/cfg/gator" {
		t.Errorf("got %s", got)
	}
}

func TestNormalizeServer(t *testing.T) {
	cases := map[string]string{
		"gator.example.com":          "https://gator.example.com",
		"https://gator.example.com/": "https://gator.example.com",
		"http://127.0.0.1:8080":      "http://127.0.0.1:8080",
		" https://x.dev/gator/ ":     "https://x.dev/gator",
	}
	for in, want := range cases {
		if got, err := NormalizeServer(in); err != nil || got != want {
			t.Errorf("NormalizeServer(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "https://"} {
		if _, err := NormalizeServer(bad); err == nil {
			t.Errorf("NormalizeServer(%q) accepted", bad)
		}
	}
}

func TestFlagsAfterArguments(t *testing.T) {
	fs, g := newFlags("x")
	kind := fs.String("kind", "", "")
	pos, err := parse(fs, []string{"Fix", "--kind", "bug", "login", "--json", "--", "--literal"})
	if err != nil {
		t.Fatal(err)
	}
	if *kind != "bug" || !g.json || strings.Join(pos, "|") != "Fix|login|--literal" {
		t.Errorf("kind=%q json=%v pos=%v", *kind, g.json, pos)
	}
}
