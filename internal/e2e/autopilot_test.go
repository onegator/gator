package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/events"
)

// recordingExec keeps every job it received and answers like the role would.
type recordingExec struct {
	mu   sync.Mutex
	jobs []proto.Job
	fail bool
}

func (r *recordingExec) Run(_ context.Context, job proto.Job, io client.JobIO) proto.Finish {
	r.mu.Lock()
	r.jobs = append(r.jobs, job)
	fail := r.fail
	r.mu.Unlock()
	io.Emit("text", map[string]string{"text": "working as " + job.Role})
	if fail {
		return proto.Finish{Status: proto.StatusFailed, StopReason: "could not do it", Usage: proto.Usage{DurationMS: 5}}
	}
	summary := map[string]string{"researcher": "## Problem\nBRIEF", "planner": "## Goal\nPLAN", "worker": "## What changed\nREPORT"}[job.Role]
	return proto.Finish{Status: proto.StatusDone, Summary: summary, Usage: proto.Usage{Backend: job.Backend, DurationMS: 1000, InputTokens: 10}}
}

// last is the newest job this executor ran; it fails the test rather than panicking when
// another runner took the work.
func (r *recordingExec) last(t *testing.T) proto.Job {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.jobs) == 0 {
		t.Fatal("this runner ran no job")
	}
	return r.jobs[len(r.jobs)-1]
}

func (h *harness) featureTask(backend string) (gen.Project, gen.Task) {
	h.t.Helper()
	var p gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("ap%d", time.Now().UnixNano()), Name: "AP"}, &p)
	cfg, _ := json.Marshal(map[string]any{"autopilot": map[string]any{"backend": backend}})
	if _, err := h.pool.Exec(context.Background(), "UPDATE projects SET process_config = $2 WHERE id = $1", p.Id.String(), string(cfg)); err != nil {
		h.t.Fatal(err)
	}
	desc := "Users want a dark mode toggle in settings."
	var task gen.Task
	if code := h.do("POST", "/projects/"+p.Id.String()+"/tasks", gen.NewTask{Kind: "feature", Title: "Dark mode", Description: &desc}, &task); code != 201 {
		h.t.Fatalf("task: %d", code)
	}
	return p, task
}

func (h *harness) approveAdvance(task gen.Task) gen.Task {
	h.t.Helper()
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/approve", nil, nil); code != 200 {
		h.t.Fatalf("approve: %d", code)
	}
	var out gen.Task
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/advance", gen.Reason{}, &out); code != 200 {
		h.t.Fatalf("advance: %d", code)
	}
	return out
}

func (h *harness) ensure(task gen.Task) (gen.Job, bool) {
	h.t.Helper()
	j, created, err := h.mgr.EnsureJob(context.Background(), pgtype.UUID{Bytes: task.Id, Valid: true})
	if err != nil {
		h.t.Fatal(err)
	}
	var out gen.Job
	if created {
		h.do("GET", "/jobs/"+uuidOf(j.ID), nil, &out)
	}
	return out, created
}

func uuidOf(u pgtype.UUID) string {
	v, _ := u.Value()
	s, _ := v.(string)
	return s
}

// artifacts returns the phase documents of a task, without its working state.
func (h *harness) artifacts(task gen.Task) []gen.Artifact {
	var all, a []gen.Artifact
	h.do("GET", "/tasks/"+task.Id.String()+"/artifacts", nil, &all)
	for _, x := range all {
		if x.Phase != "task" {
			a = append(a, x)
		}
	}
	return a
}

func (h *harness) inboxReason(p gen.Project, task gen.Task) string {
	var ds []gen.Decision
	h.do("GET", "/inbox?projectId="+p.Id.String(), nil, &ds)
	for _, d := range ds {
		if d.Task.Id == task.Id {
			return string(d.Reason)
		}
	}
	return ""
}

// A feature moves idea → discovery → planning → implementation; every runner phase gets
// exactly one job with the right role, prompt and context, and leaves its document.
func TestAutopilotRunsEachRunnerPhase(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	p, task := h.featureTask(backend)
	exec := &recordingExec{}
	h.startRunner(backend, exec)

	if _, created := h.ensure(task); created {
		t.Fatal("idea is a human phase; no job")
	}
	task = h.approveAdvance(task)
	if task.Phase != "discovery" {
		t.Fatalf("phase %s", task.Phase)
	}
	job, created := h.ensure(task)
	if !created || job.Role != "researcher" || job.Backend != backend {
		t.Fatalf("discovery job: %+v created=%v", job, created)
	}
	if _, again := h.ensure(task); again {
		t.Fatal("a second call must not queue another job for the same phase entry")
	}
	h.waitJob(job.Id.String(), "done")

	got := exec.last(t)
	if !strings.Contains(got.Guide, "# Role: researcher") || got.TaskDescription != "Users want a dark mode toggle in settings." || got.TaskTitle != "Dark mode" || len(taskDocs(got.Context)) != 0 {
		t.Fatalf("researcher lease: guide=%v desc=%q ctx=%d", strings.Contains(got.Guide, "researcher"), got.TaskDescription, len(got.Context))
	}
	arts := h.artifacts(task)
	if len(arts) != 1 || arts[0].Type != "brief" || arts[0].Approved || arts[0].Content == nil || !strings.Contains(*arts[0].Content, "BRIEF") {
		t.Fatalf("brief artifact: %+v", arts)
	}
	if r := h.inboxReason(p, task); r != "approval" {
		t.Fatalf("after the brief the owner decides: %q", r)
	}

	task = h.approveAdvance(task)
	if a := h.artifacts(task); !a[0].Approved {
		t.Fatal("approving discovery approves its brief")
	}
	job, _ = h.ensure(task)
	if job.Role != "planner" {
		t.Fatalf("planning job role %s", job.Role)
	}
	h.waitJob(job.Id.String(), "done")
	if docs := taskDocs(exec.last(t).Context); len(docs) != 2 || docs[0].Kind != "working_state" || docs[1].Kind != "brief" || !strings.Contains(docs[1].Title, "approved") {
		t.Fatalf("planner context should be the working state, then the approved brief: %+v", docs)
	}

	task = h.approveAdvance(task)
	job, _ = h.ensure(task)
	if job.Role != "worker" || task.Phase != "implementation" {
		t.Fatalf("implementation job: %+v", job)
	}
	h.waitJob(job.Id.String(), "done")

	reason := "plan must cover the settings migration"
	var rolled gen.Task
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/rollback", gen.Rollback{To: "planning", Reason: reason}, &rolled); code != 200 {
		t.Fatalf("rollback: %d", code)
	}
	job, created = h.ensure(rolled)
	if !created || job.Role != "planner" {
		t.Fatalf("re-entering planning needs a new job: %+v %v", job, created)
	}
	h.waitJob(job.Id.String(), "done")
	ctx := exec.last(t).Context
	if len(ctx) < 4 || ctx[0].Kind != "rollback" || ctx[0].Body != reason {
		t.Fatalf("replanning context should lead with the rollback reason: %+v", ctx)
	}
	var jobs []gen.Job
	h.do("GET", "/tasks/"+task.Id.String()+"/jobs", nil, &jobs)
	if len(jobs) != 4 {
		t.Fatalf("one job per runner phase entry, got %d", len(jobs))
	}
}

func TestAutopilotOffAndFailedJobs(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	p, task := h.featureTask(backend)
	exec := &recordingExec{fail: true}
	h.startRunner(backend, exec)
	task = h.approveAdvance(task)

	job, created := h.ensure(task)
	if !created {
		t.Fatal("autopilot should queue")
	}
	h.waitJob(job.Id.String(), "failed")
	if r := h.inboxReason(p, task); r != "job_failed" {
		t.Fatalf("a failed job goes to the owner: %q", r)
	}
	if _, again := h.ensure(task); again {
		t.Fatal("autopilot must not retry a failed job by itself")
	}
	if a := h.artifacts(task); len(a) != 0 {
		t.Fatalf("a failed job leaves no artifact: %+v", a)
	}

	_, off := h.featureTask(backend)
	if _, err := h.pool.Exec(context.Background(), `UPDATE projects SET process_config = '{"autopilot":{"enabled":false}}' WHERE id = $1`, off.ProjectId.String()); err != nil {
		t.Fatal(err)
	}
	off = h.approveAdvance(off)
	if _, created := h.ensure(off); created {
		t.Fatal("autopilot off: no job")
	}
}

func TestAutopilotReactsToTaskEvents(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	_, task := h.featureTask(backend)
	task = h.approveAdvance(task) // discovery, no job yet: this harness has no relay
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.mgr.RunAutopilot(ctx)
	time.Sleep(50 * time.Millisecond)
	h.hub.Publish(events.Event{Type: "task.phase_changed", Aggregate: "task", AggregateID: task.Id.String(), Payload: []byte(`{}`)})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var jobs []gen.Job
		h.do("GET", "/tasks/"+task.Id.String()+"/jobs", nil, &jobs)
		if len(jobs) == 1 && jobs[0].Role == "researcher" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the event did not queue a job")
}

// taskDocs drops the documents that belong to the workspace rather than to this task, so a
// test can count what it is about.
func taskDocs(docs []proto.ContextDoc) []proto.ContextDoc {
	var out []proto.ContextDoc
	for _, d := range docs {
		if d.Kind != "knowledge" && d.Kind != "product" {
			out = append(out, d)
		}
	}
	return out
}
