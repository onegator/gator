package e2e

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/server/api/gen"
)

// tokenExec keeps the job it was handed, so a test can talk back the way gator-cli does.
type tokenExec struct {
	mu   sync.Mutex
	job  proto.Job
	seen chan struct{}
	hold bool
}

func (e *tokenExec) Run(ctx context.Context, job proto.Job, io client.JobIO) proto.Finish {
	e.mu.Lock()
	e.job = job
	e.mu.Unlock()
	// A real agent says something as it starts, and that first event is what moves the job
	// from leased to running.
	io.Emit("text", map[string]string{"text": "looking at the task"})
	select {
	case e.seen <- struct{}{}:
	default:
	}
	if e.hold {
		<-ctx.Done()
	}
	return proto.Finish{Status: proto.StatusDone, Summary: "done",
		Usage: proto.Usage{Backend: job.Backend, Model: "fake-1", InputTokens: 10, OutputTokens: 5, DurationMS: 100}}
}

func (e *tokenExec) token(t *testing.T) (string, proto.Job) {
	t.Helper()
	select {
	case <-e.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner never got a job")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.job.AgentToken == "" {
		t.Fatal("a leased job should carry an identity of its own")
	}
	return e.job.AgentToken, e.job
}

// An agent that cannot go on says so, and the task waits for a person instead of the job
// burning an hour on a guess.
func TestAnAgentCanStopAndAskInsteadOfGuessing(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	exec := &tokenExec{seen: make(chan struct{}, 1), hold: true}
	_, stop := h.startRunner(backend, exec)
	defer stop()
	job := h.job(task, backend, "work", 0)
	h.waitJob(job.Id.String(), "running")
	token, leased := exec.token(t)

	// It reads its own task first.
	var about gen.AgentTask
	if code := h.asJob(token, "GET", "/agent/task", nil, &about); code != 200 {
		t.Fatalf("task show: %d", code)
	}
	if about.Task.Id != task.Id || about.Role != leased.Role {
		t.Fatalf("a job should see its own task: %+v", about)
	}

	var wrote gen.AgentWrite
	if code := h.asJob(token, "POST", "/agent/blocked",
		gen.AgentReason{Reason: "the repository has no test suite, so I cannot verify anything"}, &wrote); code != 200 {
		t.Fatalf("blocked: %d", code)
	}
	if !wrote.Ok || deref(wrote.Link) == "" {
		t.Fatalf("a write should answer with a link to open: %+v", wrote)
	}
	var after gen.TaskDetail
	h.do("GET", "/tasks/"+task.Id.String(), nil, &after)
	if after.BlockedReason == nil || !contains(*after.BlockedReason, "no test suite") {
		t.Fatalf("the task should carry the agent's words: %+v", after.BlockedReason)
	}
	// And a person is asked about it.
	found := false
	for _, d := range h.decisions("") {
		if d.Task.Id == task.Id && d.Reason == "blocked" {
			found = true
		}
	}
	if !found {
		t.Fatal("a blocked task belongs in the inbox")
	}
	// The call is in the job's own events, where somebody watching the work sees it.
	if h.count(`SELECT count(*) FROM agent_calls WHERE job_id = $1`, job.Id.String()) < 2 {
		t.Fatal("agent calls should be recorded on the job")
	}
}

// What the work taught the product is proposed, never approved: a person decides what every
// later prompt will carry.
func TestAnAgentProposesButNeverApproves(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	exec := &tokenExec{seen: make(chan struct{}, 1), hold: true}
	_, stop := h.startRunner(backend, exec)
	defer stop()
	job := h.job(task, backend, "work", 0)
	h.waitJob(job.Id.String(), "running")
	token, _ := exec.token(t)

	var wrote gen.AgentWrite
	if code := h.asJob(token, "POST", "/agent/product", gen.AgentProduct{
		Kind: "lesson", Title: "Cache invalidation", Content: "Invalidate on write; the stale read bit us twice."}, &wrote); code != 200 {
		t.Fatalf("propose: %d", code)
	}
	entries := h.productEntries(task.ProjectId.String())
	if len(entries) != 1 || entries[0].Status != "proposed" || entries[0].Kind != "lesson" {
		t.Fatalf("entries = %+v", entries)
	}
	// A document recorded mid-job survives the job.
	if code := h.asJob(token, "POST", "/agent/artifacts",
		gen.AgentArtifact{Type: "note", Content: "Found the stale read in the cache layer."}, nil); code != 200 {
		t.Fatal("artifact put")
	}
	var artifacts []gen.Artifact
	h.do("GET", "/tasks/"+task.Id.String()+"/artifacts", nil, &artifacts)
	if len(artifacts) != 1 || artifacts[0].Type != "note" {
		t.Fatalf("artifacts = %+v", artifacts)
	}
}

// A job's identity is about its own work and nothing else: not another project, and not after
// the job has ended.
func TestAJobsIdentityReachesOnlyItsOwnWork(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	exec := &tokenExec{seen: make(chan struct{}, 1), hold: true}
	runner, stop := h.startRunner(backend, exec)
	defer stop()
	job := h.job(task, backend, "work", 0)
	h.waitJob(job.Id.String(), "running")
	token, _ := exec.token(t)

	// Another project exists, with its own product context; this job cannot read it.
	other := h.oneProject("oth")
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO product_context (project_id, kind, title, content, version, status, created_by_kind, approved_at)
		 VALUES ($1, 'decision', 'Secret', 'We are acquiring Acme.', 1, 'approved', 'user', now())`, other.Id.String()); err != nil {
		t.Fatal(err)
	}
	var docs []gen.AgentDocument
	if code := h.asJob(token, "GET", "/agent/knowledge?q=Acme", nil, &docs); code != 200 {
		t.Fatalf("knowledge: %d", code)
	}
	if len(docs) != 0 {
		t.Fatalf("a job must not read another project's documents: %+v", docs)
	}
	// The whole API is not open to it either.
	if code := h.asJob(token, "GET", "/projects", nil, nil); code == 200 {
		t.Fatal("a job's identity is not a person's")
	}

	// When the job ends, the identity stops working.
	_ = runner
	if code := h.do("POST", "/jobs/"+job.Id.String()+"/stop", gen.Reason{Reason: ptr("that is enough")}, nil); code != 202 && code != 200 {
		t.Fatalf("stop: %d", code)
	}
	h.waitJob(job.Id.String(), "done", "failed", "stopped")
	deadline := time.Now().Add(3 * time.Second)
	for {
		code := h.asJob(token, "GET", "/agent/task", nil, nil)
		if code == 401 || code == 403 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a finished job's identity should stop working, got %d", code)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
