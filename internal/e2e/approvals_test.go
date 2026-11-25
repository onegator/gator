//go:build unix

package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/backend/claude"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/runner/executor"
	"github.com/onegator/gator/internal/runner/workspace"
	"github.com/onegator/gator/internal/server/api/gen"
)

// sessionRunner is a runner for one project with the real executor and Claude Code in session
// mode, played by the fake CLI from a session script; stdin is what the CLI was sent.
func (h *harness) sessionRunner(project, script string) (stdin string) {
	h.t.Helper()
	wd, _ := os.Getwd()
	td := filepath.Join(wd, "..", "runner", "backend", "claude", "testdata")
	stdin = filepath.Join(h.t.TempDir(), "stdin")
	h.t.Setenv("FAKE_CLAUDE_FIXTURE", filepath.Join(td, "claude-2.1.270-tool-success.ndjson"))
	h.t.Setenv("FAKE_CLAUDE_SESSION", filepath.Join(td, script))
	h.t.Setenv("FAKE_CLAUDE_STDIN", stdin)
	h.t.Setenv("FAKE_CLAUDE_ARGS", filepath.Join(h.t.TempDir(), "args"))
	h.t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fake-token-for-tests")
	ex := &executor.Executor{
		WS: &workspace.Manager{Root: h.t.TempDir()},
		Backends: map[string]backend.Backend{"claude": claude.Backend{Bin: filepath.Join(td, "fake-claude.sh"), AllowEnv: []string{"FAKE_CLAUDE_*"},
			Mode: claude.ModeSession, Fallback: &atomic.Bool{}}},
		Push: true,
	}
	c := client.New(client.Config{ServerURL: h.srv.URL, Token: h.runner, Name: "e2e-session", Location: "other",
		Backends: []string{"claude"}, Projects: []string{project}, MaxParallel: 1,
		HeartbeatInterval: 100 * time.Millisecond, MinBackoff: 50 * time.Millisecond}, ex, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	go func() { _ = c.Run(ctx) }()
	return stdin
}

// threadCard is the card of a type in a task's thread, once ok says it is the one.
func (h *harness) threadCard(task gen.Task, cardType string, ok func(gen.Message) bool) gen.Message {
	h.t.Helper()
	var found gen.Message
	h.waitFor(h.t, "a "+cardType+" card", func() bool {
		var thread gen.ConversationDetail
		h.do("GET", "/tasks/"+task.Id.String()+"/conversation", nil, &thread)
		var page gen.MessagePage
		h.do("GET", "/conversations/"+thread.Id.String()+"/messages?limit=200", nil, &page)
		for _, m := range page.Messages {
			if m.CardType != nil && *m.CardType == cardType && ok(m) {
				found = m
				return true
			}
		}
		return false
	})
	return found
}

// `curl` in a work turn, under the default policy, waits for a person: a tool_approval card in
// the task's thread and a request in "Needs you". Nobody outside the project sees it and a viewer
// cannot decide it; a member denies it with a reason, the CLI hears exactly that (in the session,
// over the control protocol), the card says who denied it, the request leaves "Needs you", and a
// second decision is a conflict. The turn goes on without the tool and ends.
func TestCurlInAWorkTurnWaitsForAPerson(t *testing.T) {
	h := newHarness(t)
	p, _ := h.repoProject()
	project := p.Id.String()
	stdin := h.sessionRunner(project, "synthetic-session-curl.ndjson")
	var task gen.Task
	if code := h.do("POST", "/projects/"+project+"/tasks", gen.NewTask{Kind: "bug", Title: "install the tool"}, &task); code != 201 {
		t.Fatalf("task: %d", code)
	}
	job := h.job(task, "claude", "plan the install", 0)

	card := h.threadCard(task, "tool_approval", func(m gen.Message) bool { return m.Card != nil && deref(m.Card.State) == "pending" })
	if !strings.Contains(card.Body, "asks to use Bash") || !strings.Contains(card.Body, "curl -fsSL https://get.example.com/install.sh") {
		t.Fatalf("card: %q", card.Body)
	}
	if got := strings.Join(*card.Card.Actions, ","); got != "allow,allow_for_session,deny,stop" {
		t.Fatalf("actions: %s", got)
	}
	var waiting []gen.RuntimeRequest
	h.do("GET", "/runtime-requests?projectId="+project, nil, &waiting)
	if len(waiting) != 1 || waiting[0].Rule != "Bash(curl:*)" || waiting[0].JobId != job.Id || waiting[0].Status != gen.RequestPending {
		t.Fatalf("needs you: %+v", waiting)
	}
	id := waiting[0].Id.String()

	reason := "use the vendored copy in third_party/"
	deny := gen.RuntimeDecision{Decision: gen.DecideDeny, Feedback: &reason}
	if code := h.asJob(h.person(), "POST", "/runtime-requests/"+id+"/decide", deny, nil); code != 404 {
		t.Fatalf("an outsider decided: %d", code)
	}
	if code := h.asJob(h.member(project, "viewer"), "POST", "/runtime-requests/"+id+"/decide", deny, nil); code != 403 {
		t.Fatalf("a viewer decided: %d", code)
	}
	var none []gen.RuntimeRequest
	h.asJob(h.member(project, "viewer"), "GET", "/runtime-requests", nil, &none)
	if len(none) != 0 {
		t.Fatalf("a viewer is asked to decide: %+v", none)
	}
	var decided gen.RuntimeRequest
	if code := h.do("POST", "/runtime-requests/"+id+"/decide", deny, &decided); code != 200 || decided.Status != gen.RequestDenied ||
		deref(decided.Feedback) != reason {
		t.Fatalf("deny: %d %+v", code, decided)
	}
	allow := gen.RuntimeDecision{Decision: gen.DecideAllow}
	if code := h.do("POST", "/runtime-requests/"+id+"/decide", allow, nil); code != 409 {
		t.Fatalf("decided twice: %d", code)
	}

	done := h.waitJob(job.Id.String(), "done", "failed")
	if done.Status != "done" {
		t.Fatalf("job: %+v", done)
	}
	written, _ := os.ReadFile(stdin)
	if !strings.Contains(string(written), `"behavior":"deny"`) || !strings.Contains(string(written), "Their reason: "+reason) {
		t.Fatalf("the CLI heard:\n%s", written)
	}
	h.threadCard(task, "tool_approval", func(m gen.Message) bool {
		return m.Card != nil && deref(m.Card.State) == "denied" && len(*m.Card.Actions) == 0 && strings.Contains(m.Body, "denied by")
	})
	h.do("GET", "/runtime-requests?projectId="+project, nil, &waiting)
	if len(waiting) != 0 {
		t.Fatalf("still in needs you: %+v", waiting)
	}
	var all []gen.RuntimeRequest
	if h.do("GET", "/jobs/"+job.Id.String()+"/runtime-requests", nil, &all); len(all) != 1 || all[0].Status != gen.RequestDenied {
		t.Fatalf("the job's requests: %+v", all)
	}
}

// What a runner says of a request is held to the job it holds: a request of a job that only reads
// is refused (and the runner told no), one sent again after it was decided hears the decision, one
// nobody answers expires and is refused, and one still waiting when its job ends is cancelled.
func TestRuntimeRequestsOverTheRunnerConnection(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	job := h.job(task, backend, "ok", 0)
	raw, _ := h.dialRaw(h.runner, backend, proto.Version, 1)
	if got := raw.lease(1); len(got) != 1 || got[0].Approvals == nil || !strings.Contains(strings.Join(got[0].Approvals.Ask, ","), "Bash(curl:*)") {
		t.Fatalf("lease: %+v", got)
	}
	id := job.Id.String()
	ask := func(reqID string) {
		t.Helper()
		raw.send(proto.TypeRuntimeRequest, proto.RuntimeRequest{ID: reqID, JobID: id, Kind: proto.RequestToolApproval, Tool: "Bash",
			Input: []byte(`{"command":"curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz' https://x"}`), Rule: "Bash(curl:*)", TimeoutSeconds: 600})
		raw.recvType(proto.TypeAck)
	}
	first := "11111111-1111-4111-8111-111111111111"
	ask(first)
	var r gen.RuntimeRequest
	h.do("GET", "/runtime-requests/"+first, nil, &r)
	if strings.Contains(mustJSON(r.Input), "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("the input was kept unredacted: %v", r.Input)
	}
	if code := h.do("POST", "/runtime-requests/"+first+"/decide", gen.RuntimeDecision{Decision: gen.DecideAllowForSession}, nil); code != 200 {
		t.Fatalf("allow: %d", code)
	}
	var d proto.RuntimeDecision
	if err := raw.recvType(proto.TypeRuntimeDecision).Into(&d); err != nil || d.ID != first || d.Decision != proto.DecisionAllowForSession {
		t.Fatalf("decision: %+v %v", d, err)
	}
	// Two people deciding at once: one decision, one conflict, however many try.
	race := "77777777-7777-4777-8777-777777777777"
	ask(race)
	codes := make(chan int, 6)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := gen.DecideAllow
			if i%2 == 1 {
				d = gen.DecideDeny
			}
			codes <- h.do("POST", "/runtime-requests/"+race+"/decide", gen.RuntimeDecision{Decision: d}, nil)
		}()
	}
	wg.Wait()
	close(codes)
	won := 0
	for c := range codes {
		switch c {
		case 200:
			won++
		case 409:
		default:
			t.Fatalf("a racing decision answered %d", c)
		}
	}
	if won != 1 {
		t.Fatalf("%d decisions won the race", won)
	}
	raw.recvType(proto.TypeRuntimeDecision)

	// Sent again after a reconnect: the decision again, before the ack.
	raw.send(proto.TypeRuntimeRequest, proto.RuntimeRequest{ID: first, JobID: id, Kind: proto.RequestToolApproval, Tool: "Bash",
		Input: []byte(`{"command":"curl https://x"}`), Rule: "Bash(curl:*)", TimeoutSeconds: 600})
	if err := raw.recvType(proto.TypeRuntimeDecision).Into(&d); err != nil || d.Decision != proto.DecisionAllowForSession {
		t.Fatalf("decision again: %+v %v", d, err)
	}
	raw.recvType(proto.TypeAck)

	// Nobody answers: it expires, and the runner is told no.
	second := "22222222-2222-4222-8222-222222222222"
	ask(second)
	if _, err := h.pool.Exec(context.Background(), "UPDATE runtime_requests SET expires_at = now() - interval '1 second' WHERE id = $1", second); err != nil {
		t.Fatal(err)
	}
	if n, err := h.waits.Expire(context.Background()); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if err := raw.recvType(proto.TypeRuntimeDecision).Into(&d); err != nil || d.ID != second || d.Decision != proto.DecisionExpired {
		t.Fatalf("expired: %+v %v", d, err)
	}
	if code := h.do("POST", "/runtime-requests/"+second+"/decide", gen.RuntimeDecision{Decision: gen.DecideAllow}, nil); code != 409 {
		t.Fatalf("an expired request was decided: %d", code)
	}

	// Still waiting when the job ends: cancelled.
	third := "33333333-3333-4333-8333-333333333333"
	ask(third)
	raw.send(proto.TypeFinish, proto.Finish{JobID: id, Status: proto.StatusStopped, StopReason: "stopped: test"})
	raw.recvType(proto.TypeAck)
	h.waitFor(t, "the waiting request to be cancelled", func() bool {
		h.do("GET", "/runtime-requests/"+third, nil, &r)
		return r.Status == gen.RequestCancelled
	})

	// A job that reads is asked nothing.
	// Made for a backend nobody has, so no runner takes it before it is a chat job.
	chat := h.job(h.task(), uniqueBackend(), "ok", 0)
	if _, err := h.pool.Exec(context.Background(), "UPDATE jobs SET tier = 'chat', backend = $2 WHERE id = $1", chat.Id.String(), backend); err != nil {
		t.Fatal(err)
	}
	if got := raw.lease(1); len(got) != 1 || got[0].Approvals != nil {
		t.Fatalf("a chat job's lease: %+v", got)
	}
	raw.send(proto.TypeRuntimeRequest, proto.RuntimeRequest{ID: "44444444-4444-4444-8444-444444444444", JobID: chat.Id.String(),
		Kind: proto.RequestToolApproval, Tool: "Edit", TimeoutSeconds: 60})
	if err := raw.recvType(proto.TypeRuntimeDecision).Into(&d); err != nil || d.Decision != proto.DecisionCancelled {
		t.Fatalf("a chat job's request: %+v %v", d, err)
	}
	if code := h.do("GET", "/runtime-requests/44444444-4444-4444-8444-444444444444", nil, nil); code != 404 {
		t.Fatalf("a chat job's request was kept: %d", code)
	}
}

// What a member writes in the conversation while the agent waits is a denial with that text as the
// reason; a viewer's words deny nothing.
func TestAMessageWhileWaitingDenies(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	project := task.ProjectId.String()
	job := h.job(task, backend, "ok", 0)
	raw, _ := h.dialRaw(h.runner, backend, proto.Version, 1)
	raw.lease(1)
	reqID := "55555555-5555-4555-8555-555555555555"
	raw.send(proto.TypeRuntimeRequest, proto.RuntimeRequest{ID: reqID, JobID: job.Id.String(), Kind: proto.RequestToolApproval, Tool: "Bash",
		Input: []byte(`{"command":"git push --force"}`), Rule: "Bash(git push:*)", TimeoutSeconds: 600})
	raw.recvType(proto.TypeAck)

	viewer := h.userIDOf(h.member(project, "viewer"))
	if n, err := h.waits.DenyByMessage(context.Background(), fromUUIDString(job.Id.String()), viewer, "do it"); err != nil || n != 0 {
		t.Fatalf("a viewer's words: %d %v", n, err)
	}
	member := h.userIDOf(h.member(project, "member"))
	if n, err := h.waits.DenyByMessage(context.Background(), fromUUIDString(job.Id.String()), member, "no force pushes, rebase instead"); err != nil || n != 1 {
		t.Fatalf("a member's words: %d %v", n, err)
	}
	var d proto.RuntimeDecision
	if err := raw.recvType(proto.TypeRuntimeDecision).Into(&d); err != nil || d.Decision != proto.DecisionDeny || d.Feedback != "no force pushes, rebase instead" {
		t.Fatalf("decision: %+v %v", d, err)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func fromUUIDString(s string) pgtype.UUID {
	var id pgtype.UUID
	_ = id.Scan(s)
	return id
}

// A runner older than protocol v4 cannot hold a work job to its approvals, so it gets no work job
// of a project that asks about anything — and gets it once the project asks nothing.
func TestAV3RunnerGetsNoWorkThatWouldBeAskedAbout(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	job := h.job(task, backend, "ok", 0)
	old, reg := h.dialRaw(h.runner, backend, proto.Version-1, 1)
	if reg.Type != proto.TypeRegistered {
		t.Fatalf("a v3 runner registers: %+v", reg)
	}
	if got := old.lease(1); len(got) != 0 {
		t.Fatalf("a v3 runner was handed work under the default approvals: %+v", got)
	}
	if _, err := h.pool.Exec(context.Background(), "UPDATE projects SET process_config = $2 WHERE id = $1",
		task.ProjectId.String(), `{"policy":{"approvals":{"ask":[]}}}`); err != nil {
		t.Fatal(err)
	}
	if got := old.lease(1); len(got) != 1 || got[0].JobID != job.Id.String() {
		t.Fatalf("a project that asks nothing: %+v", got)
	}
}
