package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/server/api/gen"
)

// costExec finishes every job at a fixed cost.
type costExec struct {
	rec  *recordingExec
	cost float64
}

func (c costExec) Run(ctx context.Context, job proto.Job, io client.JobIO) proto.Finish {
	f := c.rec.Run(ctx, job, io)
	f.Usage.CostUSD = c.cost
	return f
}

// authExec reports one backend's login state, switchable mid-test.
type authExec struct {
	*recordingExec
	backend string
	mu      sync.Mutex
	state   string
}

func (a *authExec) AuthState() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]string{a.backend: a.state}
}

func (a *authExec) set(s string) { a.mu.Lock(); a.state = s; a.mu.Unlock() }

// projectTask creates a project with the given process_config and a feature task in discovery.
func (h *harness) projectTask(cfg map[string]any) (gen.Project, gen.Task) {
	h.t.Helper()
	var p gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("po%d", time.Now().UnixNano()), Name: "Policy"}, &p)
	b, _ := json.Marshal(cfg)
	if _, err := h.pool.Exec(context.Background(), "UPDATE projects SET process_config = $2 WHERE id = $1", p.Id.String(), string(b)); err != nil {
		h.t.Fatal(err)
	}
	desc := "Users want to search their notes."
	var task gen.Task
	if code := h.do("POST", "/projects/"+p.Id.String()+"/tasks", gen.NewTask{Kind: "feature", Title: "Search", Description: &desc}, &task); code != 201 {
		h.t.Fatalf("task: %d", code)
	}
	return p, h.approveAdvance(task) // idea is a human phase
}

func TestPolicyChoosesBackendModelAndCostCapPerRole(t *testing.T) {
	h := newHarness(t)
	b := uniqueBackend()
	_, task := h.projectTask(map[string]any{"policy": map[string]any{
		"default": map[string]any{"backend": b, "model": "big", "max_cost_usd": 3},
		"roles":   map[string]any{"researcher": map[string]any{"model": "small", "max_cost_usd": 0.5}},
	}})
	job, created := h.ensure(task)
	if !created || job.Backend != b || deref(job.Model) != "small" {
		t.Fatalf("researcher job: %+v created=%v", job, created)
	}
	rec := &recordingExec{}
	h.startRunner(b, rec)
	h.waitJob(job.Id.String(), "done")
	if got := rec.last(t); got.Model != "small" || got.Bounds.MaxCostUSD != 0.5 {
		t.Fatalf("lease: model %q bounds %+v", got.Model, got.Bounds)
	}
	// A person's job on another backend does not inherit a model meant for this one.
	other := uniqueBackend()
	if j := h.job(task, other, "look again", 0); j.Backend != other || j.Model != nil {
		t.Fatalf("manual job: %+v", j)
	}
}

func TestLoggedOutBackendKeepsTheJobQueued(t *testing.T) {
	h := newHarness(t)
	b := uniqueBackend()
	ex := &authExec{recordingExec: &recordingExec{}, backend: b, state: "expired"}
	h.startRunner(b, ex)
	time.Sleep(300 * time.Millisecond) // the first heartbeats report the login state
	j := h.job(h.task(), b, "fix it", 0)
	time.Sleep(500 * time.Millisecond)
	var got gen.Job
	h.do("GET", "/jobs/"+j.Id.String(), nil, &got)
	if got.Status != "queued" {
		t.Fatalf("a logged-out backend must not get jobs: %s", got.Status)
	}
	ex.set("ok")
	h.waitJob(j.Id.String(), "done")
}

func TestDailyBudgetStopsNewJobsUntilRaised(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	b := uniqueBackend()
	p, task := h.projectTask(map[string]any{"autopilot": map[string]any{"backend": b}, "policy": map[string]any{"daily_budget_usd": 1}})
	h.startRunner(b, costExec{rec: &recordingExec{}, cost: 1.5})
	job, created := h.ensure(task)
	if !created {
		t.Fatal("within budget the autopilot queues")
	}
	h.waitJob(job.Id.String(), "done")

	// Spent: a person's job is refused...
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/jobs", gen.NewJob{Backend: &b}, nil); code != 409 {
		t.Fatalf("job over budget: %d", code)
	}
	// ...and the autopilot fails the gate's budget check instead of queuing.
	task = h.approveAdvance(task)
	if _, created := h.ensure(task); created {
		t.Fatal("over budget the autopilot must not queue")
	}
	var tk map[string]any
	h.do("GET", "/tasks/"+task.Id.String(), nil, &tk)
	if r, _ := tk["blockedReason"].(string); !strings.Contains(r, "budget") {
		t.Fatalf("blocked reason: %v", tk["blockedReason"])
	}
	if r := h.inboxReason(p, task); r != "blocked" {
		t.Fatalf("the owner sees the block: %q", r)
	}

	// A higher limit releases the task on the next reconcile.
	if _, err := h.pool.Exec(ctx, `UPDATE projects SET process_config = jsonb_set(process_config, '{policy,daily_budget_usd}', '10') WHERE id = $1`, p.Id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgr.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var jobs []gen.Job
	h.do("GET", "/tasks/"+task.Id.String()+"/jobs", nil, &jobs)
	planner := false
	for _, j := range jobs {
		planner = planner || j.Role == "planner"
	}
	var after map[string]any // fresh: decoding into tk would keep its old blockedReason
	h.do("GET", "/tasks/"+task.Id.String(), nil, &after)
	if !planner || after["blockedReason"] != nil {
		t.Fatalf("after raising the budget: planner job %v, blocked %v", planner, after["blockedReason"])
	}
}
