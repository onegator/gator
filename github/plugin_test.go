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
}

func newFakeGitHub(t *testing.T, token string) *fakeGitHub {
	f := &fakeGitHub{token: token, bodies: map[int]string{}, bases: map[int]string{}, labels: map[int][]string{}, runs: map[string][]CheckRun{}}
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
