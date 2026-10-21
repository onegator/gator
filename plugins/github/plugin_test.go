package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/plugin"
	"github.com/onegator/gator/plugin/plugintest"
)

var built struct {
	once sync.Once
	path string
	err  error
}

func pluginBinary(t *testing.T) string {
	t.Helper()
	built.once.Do(func() {
		dir, err := os.MkdirTemp("", "gator-plugin-github")
		if err != nil {
			built.err = err
			return
		}
		built.path = filepath.Join(dir, "github")
		if out, err := exec.Command("go", "build", "-o", built.path, ".").CombinedOutput(); err != nil {
			built.err = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if built.err != nil {
		t.Fatal(built.err)
	}
	return built.path
}

// fakeGitHub is the slice of the REST API the plugin uses.
type fakeGitHub struct {
	*httptest.Server
	token string

	mu       sync.Mutex
	requests []string
	prs      []PR
	bodies   map[int]string
	bases    map[int]string
	labels   map[int][]string
	runs     map[string][]CheckRun
	issues   []Issue
	hooks    []Hook
	secrets  map[int64]string // webhook secrets by hook id
}

func newFakeGitHub(t *testing.T, token string) *fakeGitHub {
	f := &fakeGitHub{token: token, bodies: map[int]string{}, bases: map[int]string{}, labels: map[int][]string{}, runs: map[string][]CheckRun{},
		secrets: map[int64]string{}}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"full_name": "acme/app", "default_branch": "main"})
	})
	mux.HandleFunc("POST /repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Title, Head, Base, Body string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, pr := range f.prs {
			if pr.Head.Ref == in.Head && pr.State == "open" {
				reply(w, 422, map[string]any{"message": "Validation Failed", "errors": []map[string]string{{"message": "A pull request already exists for acme:" + in.Head + "."}}})
				return
			}
		}
		pr := PR{Number: len(f.prs) + 1, Title: in.Title, State: "open", HTMLURL: fmt.Sprintf("https://github.com/acme/app/pull/%d", len(f.prs)+1)}
		pr.Head.Ref, pr.Head.SHA = in.Head, "abc123"
		f.prs = append(f.prs, pr)
		f.bodies[pr.Number], f.bases[pr.Number] = in.Body, in.Base
		reply(w, 201, pr)
	})
	mux.HandleFunc("GET /repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		head := r.URL.Query().Get("head")
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []PR{}
		for _, pr := range f.prs {
			if "acme:"+pr.Head.Ref == head && pr.State == r.URL.Query().Get("state") {
				out = append(out, pr)
			}
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("PATCH /repos/acme/app/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		var in struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.bodies[n] = in.Body
		reply(w, 200, f.prs[n-1])
	})
	mux.HandleFunc("POST /repos/acme/app/issues/{n}/labels", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		var in struct{ Labels []string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, l := range in.Labels {
			if !slices.Contains(f.labels[n], l) {
				f.labels[n] = append(f.labels[n], l)
			}
		}
		reply(w, 200, []any{})
	})
	mux.HandleFunc("DELETE /repos/acme/app/issues/{n}/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		f.mu.Lock()
		defer f.mu.Unlock()
		i := slices.Index(f.labels[n], r.PathValue("name"))
		if i < 0 {
			reply(w, 404, map[string]string{"message": "Label does not exist"})
			return
		}
		f.labels[n] = slices.Delete(f.labels[n], i, i+1)
		reply(w, 200, []any{})
	})
	mux.HandleFunc("GET /repos/acme/app/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply(w, 200, map[string]any{"total_count": len(f.runs[r.PathValue("sha")]), "check_runs": f.runs[r.PathValue("sha")]})
	})
	mux.HandleFunc("GET /repos/acme/app/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		per, _ := strconv.Atoi(q.Get("per_page"))
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []Issue{}
		for _, is := range f.issues {
			if is.State == q.Get("state") && (q.Get("labels") == "" || is.has(q.Get("labels"))) {
				out = append(out, is)
			}
		}
		from, to := min((page-1)*per, len(out)), min(page*per, len(out))
		reply(w, 200, out[from:to])
	})
	mux.HandleFunc("GET /repos/acme/app/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, is := range f.issues {
			if is.Number == n {
				reply(w, 200, is)
				return
			}
		}
		reply(w, 404, map[string]string{"message": "Not Found"})
	})
	mux.HandleFunc("PATCH /repos/acme/app/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		var in struct {
			State       string `json:"state"`
			StateReason string `json:"state_reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := range f.issues {
			if f.issues[i].Number == n {
				f.issues[i].State, f.issues[i].StateReason = in.State, in.StateReason
				reply(w, 200, f.issues[i])
				return
			}
		}
		reply(w, 404, map[string]string{"message": "Not Found"})
	})
	mux.HandleFunc("GET /repos/acme/app/hooks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply(w, 200, append([]Hook{}, f.hooks...))
	})
	saveHook := func(r *http.Request, id int64) Hook {
		var in struct {
			Events []string `json:"events"`
			Config struct {
				URL         string `json:"url"`
				Secret      string `json:"secret"`
				ContentType string `json:"content_type"`
			} `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		h := Hook{ID: id, Active: true, Events: in.Events}
		h.Config.URL = in.Config.URL
		f.secrets[id] = in.Config.Secret + "/" + in.Config.ContentType
		return h
	}
	mux.HandleFunc("POST /repos/acme/app/hooks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		h := saveHook(r, int64(len(f.hooks)+1))
		f.hooks = append(f.hooks, h)
		reply(w, 201, h)
	})
	mux.HandleFunc("PATCH /repos/acme/app/hooks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hooks[id-1] = saveHook(r, id)
		reply(w, 200, f.hooks[id-1])
	})
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			reply(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func run(t *testing.T, s plugintest.Scenario) plugintest.Report {
	t.Helper()
	rep, err := plugintest.Run(context.Background(), []string{pluginBinary(t)}, s, os.Stderr, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() {
		b, _ := json.MarshalIndent(rep.Steps, "", "  ")
		t.Fatalf("scenario failed:\n%s", b)
	}
	return rep
}

func scenario(t *testing.T, raw string, args ...any) plugintest.Scenario {
	t.Helper()
	var s plugintest.Scenario
	if err := json.Unmarshal([]byte(fmt.Sprintf(raw, args...)), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The shipped webhook scenario, which needs no GitHub API.
func TestWebhookScenario(t *testing.T) {
	s, err := plugintest.LoadScenario("scenarios/webhooks.json")
	if err != nil {
		t.Fatal(err)
	}
	rep := run(t, s)
	var issue *plugintest.Task
	for i := range rep.Tasks {
		if rep.Tasks[i].ExternalRefs[refIssue] == "acme/app#12" {
			issue = &rep.Tasks[i]
		}
	}
	if len(rep.Tasks) != 2 || issue == nil || issue.Kind != "bug" || issue.Title != "Login fails after password reset" || issue.Phase != "planning" {
		t.Fatalf("one bug task per issue: %+v", rep.Tasks)
	}
	if c := rep.Checks["t1"]; len(c) != 1 || c[0].Name != "github/ci" || c[0].Status != "pass" {
		t.Fatalf("the last check run wins: %+v", c)
	}
	var info prInfo
	_ = json.Unmarshal(rep.KV["pr:acme/app#1"], &info)
	if info.State != "merged" || len(rep.Artifacts) != 1 {
		t.Fatalf("merged pull request: %+v %+v", info, rep.Artifacts)
	}
	if !strings.Contains(string(rep.Steps[8].Result), "PR #1") || !strings.Contains(string(rep.Steps[8].Result), prColors["merged"]) {
		t.Fatalf("renderUI: %s", rep.Steps[8].Result)
	}
}

// A finished worker job opens one pull request with its report, labels follow the phase,
// and the gate gets the pull request's check runs.
func TestJobFinishOpensOnePullRequest(t *testing.T) {
	gh := newFakeGitHub(t, "ghp-test")
	gh.runs["abc123"] = []CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}, {Name: "lint", Status: "in_progress"}}
	s := scenario(t, `{
		"config": {"repo": "acme/app", "api_url": %q},
		"secrets": {"token": "ghp-test", "webhook_secret": "s"},
		"tasks": [{"id": "t1", "kind": "feature", "title": "Search", "phase": "implementation",
		           "external_refs": {"github_issue": "acme/app#7", "github_issue_url": "https://github.com/acme/app/issues/7"}}],
		"steps": [
			{"hook": "phaseTransition", "task": "t1", "from": "planning", "to": "implementation"},
			{"hook": "jobFinish", "task": "t1", "receipt": {"status": "done", "branch": "gator/t1-job1", "commits": ["abc123"], "summary": "## What changed\nAdded search."},
			 "expect_core": ["task.update", "kv.put", "artifact.put"]},
			{"hook": "jobFinish", "task": "t1", "receipt": {"status": "done", "branch": "gator/t1-job1", "commits": ["abc123", "def456"], "summary": "## What changed\nAdded search and paging."},
			 "expect_core": ["task.update"]},
			{"hook": "gateEvaluate", "task": "t1"},
			{"hook": "renderUI", "task": "t1"},
			{"hook": "jobFinish", "task": "t1", "job": {"id": "j2", "role": "reviewer", "phase": "verification", "backend": "claude", "status": "done"},
			 "receipt": {"status": "done", "summary": "LGTM"}}
		]}`, gh.URL)
	before := 0
	rep := run(t, s)

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.prs) != 1 || gh.prs[0].Head.Ref != "gator/t1-job1" || gh.bases[1] != "main" {
		t.Fatalf("one pull request from the job branch to the default branch: %+v %v", gh.prs, gh.bases)
	}
	if b := gh.bodies[1]; !strings.Contains(b, "Added search and paging.") || !strings.Contains(b, "Closes #7") || !strings.Contains(b, "2 commit(s)") {
		t.Fatalf("the retry refreshes the body with the latest report:\n%s", b)
	}
	if !slices.Equal(gh.labels[7], []string{"gator:implementation"}) || !slices.Equal(gh.labels[1], []string{"gator:implementation"}) {
		t.Fatalf("labels: %v", gh.labels)
	}
	task := rep.Tasks[0]
	if task.ExternalRefs[refPR] != "acme/app#1" || task.ExternalRefs[refBranch] != "gator/t1-job1" {
		t.Fatalf("refs: %+v", task.ExternalRefs)
	}
	checks := map[string]string{}
	for _, c := range rep.Checks["t1"] {
		checks[c.Name] = c.Status
	}
	if checks["github/ci"] != "pass" || checks["github/lint"] != "pending" {
		t.Fatalf("gate checks: %v", checks)
	}
	if !strings.Contains(string(rep.Steps[4].Result), "PR #1") || !strings.Contains(string(rep.Steps[4].Result), "acme/app#7") {
		t.Fatalf("renderUI: %s", rep.Steps[4].Result)
	}
	if n := len(rep.Steps[5].CoreCalls); n != before {
		t.Fatalf("a reviewer's job opens nothing: %d core calls", n)
	}
}

// A token GitHub refuses is this plugin's own fault and stays broken until a person fixes it.
// It used to be reported as a bad request, which is not counted toward the breaker, so the
// plugin failed on every call while the only trace was a line in the server log. An internal
// error disables it after five tries with the reason shown where it is configured.
func TestARefusedTokenDisablesThePlugin(t *testing.T) {
	gh := newFakeGitHub(t, "the-right-token")
	s := scenario(t, `{
		"config": {"repo": "acme/app", "api_url": %q},
		"secrets": {"token": "wrong", "webhook_secret": "s"},
		"tasks": [{"id": "t1", "kind": "feature", "title": "Search", "phase": "implementation"}],
		"steps": [{"hook": "jobFinish", "task": "t1", "receipt": {"status": "done", "branch": "gator/t1-j", "commits": ["a"]}, "expect_error": true}]
	}`, gh.URL)
	rep := run(t, s)
	e := rep.Steps[0].Error
	if !strings.Contains(e, "-32603") || !strings.Contains(e, "Bad credentials") {
		t.Fatalf("error: %s", e)
	}
	// The message has to name the permission, because "403" sends a person hunting.
	if !strings.Contains(e, "Checks") {
		t.Fatalf("the error should say which permission is missing: %s", e)
	}
}

func TestHelpers(t *testing.T) {
	for _, c := range []struct{ status, conclusion, want string }{
		{"queued", "", "pending"}, {"in_progress", "", "pending"}, {"completed", "success", "pass"}, {"completed", "skipped", "pass"},
		{"completed", "neutral", "pass"}, {"completed", "failure", "fail"}, {"completed", "timed_out", "fail"}, {"completed", "cancelled", "fail"},
	} {
		if got := checkStatus(c.status, c.conclusion); got != c.want {
			t.Errorf("%s/%s: %s, want %s", c.status, c.conclusion, got, c.want)
		}
	}
	body := []byte(`{"a":1}`)
	if !validSignature("k", body, plugintest.Sign("k", body, "sha256=")) || validSignature("k", body, plugintest.Sign("other", body, "sha256=")) ||
		validSignature("", body, "sha256=") || validSignature("k", body, "") {
		t.Fatal("signature check")
	}
	if n, ok := refNumber("acme/app#12", "ACME/app"); !ok || n != 12 {
		t.Fatal("ref of the repo")
	}
	if _, ok := refNumber("acme/other#12", "acme/app"); ok {
		t.Fatal("ref of another repo")
	}
}

func (f *fakeGitHub) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func issue(n int, title string, labels ...string) Issue {
	is := Issue{Number: n, Title: title, State: "open", HTMLURL: fmt.Sprintf("https://github.com/acme/app/issues/%d", n)}
	for _, l := range labels {
		is.Labels = append(is.Labels, struct {
			Name string `json:"name"`
		}{l})
	}
	return is
}

// Switching the plugin on brings the open issues in, once: a second import finds them all on
// the board already. Pull requests, which the issues API also lists, are not issues.
func TestImportBringsOpenIssuesInOnce(t *testing.T) {
	gh := newFakeGitHub(t, "tok")
	for n := 1; n <= 120; n++ {
		gh.issues = append(gh.issues, issue(n, fmt.Sprintf("Issue %d", n)))
	}
	gh.issues[0].Labels = append(gh.issues[0].Labels, struct {
		Name string `json:"name"`
	}{"bug"})
	gh.issues[1].PullRequest = &struct{}{}
	gh.issues[2].State = "closed"
	s := scenario(t, `{
		"config": {"repo": "acme/app", "api_url": %q},
		"secrets": {"token": "tok"},
		"steps": [{"hook": "import"}, {"hook": "import"}]
	}`, gh.URL)
	rep := run(t, s)
	var first, second plugin.ImportResult
	_ = json.Unmarshal(rep.Steps[0].Result, &first)
	_ = json.Unmarshal(rep.Steps[1].Result, &second)
	if first.Created != 118 || first.Existing != 0 || second.Created != 0 || second.Existing != 118 || len(rep.Tasks) != 118 {
		t.Fatalf("import: %+v then %+v, %d tasks", first, second, len(rep.Tasks))
	}
	bugs := 0
	for _, task := range rep.Tasks {
		if task.Kind == "bug" {
			bugs++
			if task.ExternalRefs[refIssue] != "acme/app#1" || !strings.Contains(task.Description, "issues/1") {
				t.Fatalf("bug task: %+v", task)
			}
		}
	}
	if bugs != 1 {
		t.Fatalf("%d bug tasks", bugs)
	}
}

func TestImportFollowsTheTriggerLabel(t *testing.T) {
	gh := newFakeGitHub(t, "tok")
	gh.issues = []Issue{issue(1, "Wanted", "gator"), issue(2, "Not this one")}
	rep := run(t, scenario(t, `{
		"config": {"repo": "acme/app", "api_url": %q, "trigger_label": "gator"},
		"secrets": {"token": "tok"},
		"steps": [{"hook": "import"}]
	}`, gh.URL))
	if len(rep.Tasks) != 1 || rep.Tasks[0].Title != "Wanted" {
		t.Fatalf("tasks: %+v", rep.Tasks)
	}
	// Without a token there is nothing to read, and the person is told what to do.
	rep, err := plugintest.Run(context.Background(), []string{pluginBinary(t)}, scenario(t, `{
		"config": {"repo": "acme/app"}, "steps": [{"hook": "import", "expect_error": true}]}`), os.Stderr, 10*time.Second)
	if err != nil || rep.Failed() || !strings.Contains(rep.Steps[0].Error, "connect GitHub") {
		t.Fatalf("no token: %v %+v", err, rep.Steps)
	}
}

// A finished task closes its issue; an issue already closed, which is what a task closed from
// GitHub's side finds, is left alone.
func TestATaskClosedClosesItsIssue(t *testing.T) {
	gh := newFakeGitHub(t, "tok")
	gh.issues = []Issue{issue(7, "Search")}
	run(t, scenario(t, `{
		"config": {"repo": "acme/app", "api_url": %q},
		"secrets": {"token": "tok"},
		"tasks": [{"id": "t1", "kind": "feature", "title": "Search", "external_refs": {"github_issue": "acme/app#7"}},
		          {"id": "t2", "kind": "feature", "title": "Elsewhere", "external_refs": {"github_issue": "other/repo#7"}}],
		"steps": [{"hook": "taskClosed", "task": "t1"}, {"hook": "taskClosed", "task": "t1"}, {"hook": "taskClosed", "task": "t2"}]
	}`, gh.URL))
	if gh.issues[0].State != "closed" || gh.issues[0].StateReason != "completed" || gh.count("PATCH /repos/acme/app/issues/7") != 1 {
		t.Fatalf("issue: %+v, %d closes", gh.issues[0], gh.count("PATCH /repos/acme/app/issues/7"))
	}
}

// Edited, closed and reopened on GitHub means the same on the board, except that a task with
// Gator's own pull request finishes by Gator's process, not by its issue closing.
func TestIssueSyncScenario(t *testing.T) {
	s, err := plugintest.LoadScenario("scenarios/issue-sync.json")
	if err != nil {
		t.Fatal(err)
	}
	rep := run(t, s)
	byRef := map[string]plugintest.Task{}
	for _, task := range rep.Tasks {
		byRef[task.ExternalRefs[refIssue]] = task
	}
	is := byRef["acme/app#12"]
	if is.Title != "Login fails after a password reset" || !strings.Contains(is.Description, "Seen on Safari") || is.Closed {
		t.Fatalf("issue task: %+v", is)
	}
	if byRef["acme/app#7"].Closed {
		t.Fatal("a task with Gator's pull request closed with its issue")
	}
	if !slices.ContainsFunc(rep.Steps[2].CoreCalls, func(c plugintest.Call) bool {
		return c.Method == "task.close" && strings.Contains(string(c.Params), "not planned")
	}) {
		t.Fatalf("close reason: %+v", rep.Steps[2].CoreCalls)
	}
}

// With the server's webhook address the plugin registers the repository webhook itself, and on
// the next start it rewrites the one it made instead of adding another.
func TestThePluginRegistersItsWebhook(t *testing.T) {
	gh := newFakeGitHub(t, "tok")
	start := func(want string) {
		p, err := plugintest.Start(context.Background(), []string{pluginBinary(t)}, plugintest.NewCore(""), plugintest.Options{
			ProjectSlug: "demo", Config: map[string]any{"repo": "acme/app", "api_url": gh.URL},
			Secrets: map[string]string{"token": "tok", "webhook_secret": "whsec"}, WebhookURL: "https://gator.example/hooks/demo/github"})
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		deadline := time.Now().Add(5 * time.Second)
		for gh.count(want) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}
	start("POST /repos/acme/app/hooks")
	start("PATCH /repos/acme/app/hooks/1")
	if gh.count("POST /repos/acme/app/hooks") != 1 || gh.count("PATCH /repos/acme/app/hooks/1") != 1 {
		t.Fatalf("requests: %v", gh.requests)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.hooks) != 1 || gh.hooks[0].Config.URL != "https://gator.example/hooks/demo/github" || gh.secrets[1] != "whsec/json" ||
		!slices.Equal(gh.hooks[0].Events, webhookEvents) {
		t.Fatalf("hooks: %+v %v", gh.hooks, gh.secrets)
	}
}
