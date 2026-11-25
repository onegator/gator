//go:build unix

package e2e

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/backend/claude"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/runner/executor"
	"github.com/onegator/gator/internal/runner/workspace"
	"github.com/onegator/gator/internal/server/api/gen"
)

// fakeModels stands in for Anthropic's and OpenAI's APIs: a check's one call lands here.
type fakeModels struct {
	srv    *httptest.Server
	mu     sync.Mutex
	status int
	seen   []string // the credentials it was called with
}

func newFakeModels(t *testing.T) *fakeModels {
	f := &fakeModels{status: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.seen = append(f.seen, r.Header.Get("x-api-key")+r.Header.Get("Authorization"))
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

const (
	annsToken    = "sk-ant-oat01-AnnsOwnClaudeSubscriptionToken0123456789_-ann1"
	runnersToken = "sk-ant-oat01-TheRunnersOwnLoginThatMustNotBeUsed0123456789"
)

// A person connects their Claude subscription; their job, which waited saying so, then runs on it
// and on nothing of the runner's; nothing Gator keeps or shows carries the token; and the usage
// says whose login paid. Nobody else reads it, an admin included.
func TestAJobRunsOnItsPayersOwnClaudeLogin(t *testing.T) {
	h := newHarness(t)
	var policy gen.ModelCredentialPolicy
	if code := h.do("PUT", "/workspace/model-credentials", gen.ModelCredentialPolicy{RunnerLogins: false}, &policy); code != 200 || policy.RunnerLogins {
		t.Fatalf("policy: %d %+v", code, policy)
	}
	t.Cleanup(func() {
		h.do("PUT", "/workspace/model-credentials", gen.ModelCredentialPolicy{RunnerLogins: true}, nil)
	})

	wd, _ := os.Getwd()
	td := filepath.Join(wd, "..", "runner", "backend", "claude", "testdata")
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("FAKE_CLAUDE_FIXTURE", filepath.Join(td, "claude-2.1.270-tool-success.ndjson"))
	t.Setenv("FAKE_CLAUDE_RESUME_FIXTURE", filepath.Join(td, "synthetic-digest-reply.ndjson"))
	t.Setenv("FAKE_CLAUDE_ENV", envFile)
	// The agent prints the token it was given, as an agent may.
	t.Setenv("FAKE_CLAUDE_SUBST", `s/"result":"DONE/"result":"DONE with `+annsToken+`/`)
	// The runner is logged in itself; a job on its payer's login must not use that.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", runnersToken)
	ex := &executor.Executor{WS: &workspace.Manager{Root: t.TempDir()},
		Backends: map[string]backend.Backend{"claude": claude.Backend{Bin: filepath.Join(td, "fake-claude.sh"), AllowEnv: []string{"FAKE_CLAUDE_*"}}}}

	task := h.task()
	c := client.New(client.Config{ServerURL: h.srv.URL, Token: h.runner, Name: "e2e-logins", Location: "other",
		Backends: []string{"claude"}, Projects: []string{task.ProjectId.String()}, MaxParallel: 1,
		HeartbeatInterval: 100 * time.Millisecond, MinBackoff: 50 * time.Millisecond}, ex, slog.Default())
	go func() { _ = c.Run(t.Context()) }()

	job := h.job(task, "claude", "Make the change.", 0)
	if job.PayerId == nil || job.WaitingReason == nil || !strings.Contains(*job.WaitingReason, "to connect a Claude subscription or an Anthropic API key") {
		t.Fatalf("job %+v", job)
	}
	time.Sleep(300 * time.Millisecond) // the runner looks at it, and leaves it
	var waiting gen.Job
	h.do("GET", "/jobs/"+job.Id.String(), nil, &waiting)
	if waiting.Status != "queued" {
		t.Fatalf("a job with nobody to pay for it ran: %s", waiting.Status)
	}

	var saved gen.ModelCredential
	if code := h.do("PUT", "/me/credentials/claude_subscription", gen.ModelCredentialSecret{Secret: annsToken}, &saved); code != 200 {
		t.Fatalf("save: %d", code)
	}
	if saved.Hint != "…ann1" || saved.Status != "unchecked" {
		t.Fatalf("saved %+v", saved)
	}
	done := h.waitJob(job.Id.String(), "done", "failed")
	if done.Status != "done" || deref(done.CredentialKind) != "claude_subscription" || *done.PayerId != *job.PayerId {
		t.Fatalf("job %s on %v: %s", done.Status, deref(done.CredentialKind), deref(done.StopReason))
	}
	env, _ := os.ReadFile(envFile)
	if !strings.Contains(string(env), "CLAUDE_CODE_OAUTH_TOKEN="+annsToken) || strings.Contains(string(env), runnersToken) {
		t.Fatalf("the agent's environment:\n%s", env)
	}

	// Nothing kept or shown carries the token: the job, its events, the list of logins.
	var events []gen.JobEvent
	h.do("GET", "/jobs/"+job.Id.String()+"/events", nil, &events)
	var listed gen.ModelCredentials
	h.do("GET", "/me/credentials", nil, &listed)
	shown, _ := json.Marshal([]any{done, events, listed})
	if strings.Contains(string(shown), annsToken) || !strings.Contains(string(shown), "[redacted]") {
		t.Fatalf("what Gator shows:\n%s", shown)
	}
	var kept int
	_ = h.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM job_events WHERE payload::text LIKE '%'||$1||'%') +
		(SELECT count(*) FROM jobs WHERE receipt::text LIKE '%'||$1||'%') + (SELECT count(*) FROM events WHERE payload::text LIKE '%'||$1||'%') +
		(SELECT count(*) FROM user_credentials WHERE secret LIKE '%'||$1||'%')`, annsToken).Scan(&kept)
	if kept != 0 {
		t.Fatalf("the token is kept in %d rows", kept)
	}
	if len(listed.Credentials) != 1 || len(listed.Kinds) != 4 || listed.RunnerLogins || listed.Credentials[0].LastUsedAt == nil {
		t.Fatalf("listed %+v", listed)
	}

	var usage gen.ModelUsage
	h.do("GET", "/me/model-usage", nil, &usage)
	if len(usage.Items) != 1 || usage.Items[0].CredentialKind != "claude_subscription" || usage.Items[0].Jobs != 1 {
		t.Fatalf("usage %+v", usage)
	}
	var all gen.ModelUsage
	h.do("GET", "/workspace/model-usage", nil, &all)
	if len(all.Items) == 0 || all.Items[0].UserId == nil || *all.Items[0].UserId != *job.PayerId {
		t.Fatalf("workspace usage %+v", all)
	}

	// The check makes one call, with the token, to the provider: here, the fake.
	var checked gen.ModelCredential
	if code := h.do("POST", "/me/credentials/claude_subscription/check", nil, &checked); code != 200 || checked.Status != "ok" {
		t.Fatalf("check: %d %+v", code, checked)
	}
	h.models.mu.Lock()
	seen := strings.Join(h.models.seen, " ")
	h.models.mu.Unlock()
	if !strings.Contains(seen, "Bearer "+annsToken) {
		t.Fatalf("the fake provider saw %q", seen)
	}

	// Somebody else sees none of it; an admin sees that the login is there, never its value.
	bob := h.person()
	var bobs gen.ModelCredentials
	if code := h.asJob(bob, "GET", "/me/credentials", nil, &bobs); code != 200 || len(bobs.Credentials) != 0 {
		t.Fatalf("Bob's logins: %d %+v", code, bobs)
	}
	if code := h.asJob(bob, "GET", "/users/"+job.PayerId.String()+"/credentials", nil, nil); code != 403 {
		t.Fatalf("Bob read Ann's logins: %d", code)
	}
	if code := h.asJob(bob, "DELETE", "/me/credentials/claude_subscription", nil, nil); code != 404 {
		t.Fatalf("Bob removed a login: %d", code)
	}
	var seenByAdmin []gen.ModelCredential
	if code := h.do("GET", "/users/"+job.PayerId.String()+"/credentials", nil, &seenByAdmin); code != 200 || len(seenByAdmin) != 1 {
		t.Fatalf("admin: %d %+v", code, seenByAdmin)
	}
	if code := h.do("PUT", "/me/credentials/claude_subscription", gen.ModelCredentialSecret{Secret: "sk-ant-api03-not-a-subscription-0123456789abcdef"}, nil); code != 400 {
		t.Fatalf("an API key saved as a subscription: %d", code)
	}
	if code := h.do("DELETE", "/me/credentials/claude_subscription", nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
}
