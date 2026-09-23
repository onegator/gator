package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/server/api/gen"
)

// A job ends when the agent says so. These tests are about the moment just before that: the
// runner asks whether the turn may end, and something may say "not yet" while the agent is
// still in its session — which is the only time an objection is cheap.

// askingExec is an agent that always thinks it is finished and always asks anyway. It records
// what it was told, so a test can see how many times the turn was sent back to work.
type askingExec struct {
	mu         sync.Mutex
	turns      int
	objections []proto.Objection
}

func (e *askingExec) Run(_ context.Context, job proto.Job, io client.JobIO) proto.Finish {
	usage := proto.Usage{Backend: job.Backend, Model: "fake-1", InputTokens: 10, OutputTokens: 2, DurationMS: 100, CostUSD: 0.01, CostEstimated: true}
	objections := 0
	for {
		e.mu.Lock()
		e.turns++
		turn := e.turns
		e.mu.Unlock()
		io.Emit("text", map[string]string{"text": fmt.Sprintf("turn %d", turn)})
		if io.TurnEnding == nil {
			break
		}
		got := io.TurnEnding(proto.TurnEnding{JobID: job.JobID, Turn: objections + 1, Status: proto.StatusDone, Summary: "did it"})
		if len(got) == 0 {
			break
		}
		objections++
		e.mu.Lock()
		e.objections = append(e.objections, got...)
		e.mu.Unlock()
		for _, o := range got {
			io.Emit("objection", map[string]any{"source": o.Source, "reason": o.Reason, "turn": objections})
		}
		if objections > 5 { // the server is meant to stop this; do not loop for ever if it does not
			break
		}
	}
	return proto.Finish{Status: proto.StatusDone, Summary: "did it", Usage: usage, Objections: objections}
}

func (e *askingExec) counts() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.turns, len(e.objections)
}

// taskIn creates a task in a project that already exists, which the plugin tests need: the
// objection has to come from a plugin enabled on this task's own project.
func (h *harness) taskIn(projectID string) gen.Task {
	h.t.Helper()
	var task gen.Task
	if code := h.do("POST", "/projects/"+projectID+"/tasks", gen.NewTask{Kind: "bug", Title: "turn"}, &task); code != 201 {
		h.t.Fatalf("task: %d", code)
	}
	return task
}

// A plugin that is never satisfied would hold an agent in a loop and spend a person's money
// doing it. It gets two goes; the third ending stands, whatever it thinks.
func TestAnObjectionSendsTheTurnBackAndTheLimitEndsIt(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi", "object": "the tests were never run", "object_turns": "9"})
	backend := uniqueBackend()
	exec := &askingExec{}
	_, stop := h.startRunner(backend, exec)
	defer stop()

	job := h.job(h.taskIn(p.id), backend, "ok", 0)
	done := h.waitJob(job.Id.String(), "done", "failed")
	if done.Status != "done" {
		t.Fatalf("job: %+v", done)
	}
	turns, objections := exec.counts()
	if objections != 2 || turns != 3 {
		t.Fatalf("two objections and three turns, got %d objections over %d turns", objections, turns)
	}
	if got := (*done.Receipt)["objections"]; got != float64(2) {
		t.Fatalf("the receipt must say the turn was sent back twice: %v", *done.Receipt)
	}
	if n := h.count("SELECT objections FROM jobs WHERE id = $1", job.Id.String()); n != 2 {
		t.Fatalf("the server counts the objections it allowed: %d", n)
	}

	// Test two: a person reading the job sees the objection, with the reason, in the feed.
	var evs []gen.JobEvent
	h.do("GET", "/jobs/"+job.Id.String()+"/events", nil, &evs)
	found := false
	for _, e := range evs {
		if e.Type == "objection" {
			found = true
			if e.Payload["reason"] != "the tests were never run" || e.Payload["source"] != "echo" {
				t.Fatalf("the feed row must carry who objected and why: %+v", e.Payload)
			}
		}
	}
	if !found {
		t.Fatalf("no objection in the job's events: %+v", evs)
	}
}

// Nothing with an opinion means nothing changes: the turn ends when the agent says it does,
// exactly as it did before any of this existed.
func TestWithNoPluginTheTurnEndsWhenTheAgentSaysSo(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	exec := &askingExec{}
	_, stop := h.startRunner(backend, exec)
	defer stop()

	job := h.job(h.task(), backend, "ok", 0)
	done := h.waitJob(job.Id.String(), "done", "failed")
	turns, objections := exec.counts()
	if done.Status != "done" || turns != 1 || objections != 0 {
		t.Fatalf("job %+v turns %d objections %d", done, turns, objections)
	}
	if n := h.count("SELECT objections FROM jobs WHERE id = $1", job.Id.String()); n != 0 {
		t.Fatalf("nothing objected, so nothing should be counted: %d", n)
	}
}

// The one objection Gator makes on its own: the task was planned, and the turn that was
// supposed to carry the plan out committed nothing. It is deliberately the narrow case — not
// "the plan looks unfinished", which would be a guess — because it is the case where a person
// loses a whole phase to a report about work that never happened.
func TestGatorItselfObjectsToAPlannedPhaseThatCommittedNothing(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	exec := &askingExec{}
	_, stop := h.startRunner(backend, exec)
	defer stop()

	task := h.task() // a bug starts in planning
	planning := h.job(task, backend, "ok", 0)
	h.waitJob(planning.Id.String(), "done", "failed")
	if turns, objections := exec.counts(); turns != 1 || objections != 0 {
		t.Fatalf("planning is not the phase being objected to: turns %d objections %d", turns, objections)
	}
	var plan bool
	for _, a := range h.artifacts(task) {
		if a.Type == "plan" {
			plan = true
		}
	}
	if !plan {
		t.Fatalf("the planning job should have left a plan: %+v", h.artifacts(task))
	}

	moved := h.approveAdvance(task)
	if moved.Phase != "implementation" {
		t.Fatalf("phase: %s", moved.Phase)
	}
	work := h.job(moved, backend, "ok", 0)
	done := h.waitJob(work.Id.String(), "done", "failed")
	turns, objections := exec.counts()
	if objections != 2 || turns != 4 { // one planning turn plus three here
		t.Fatalf("gator should object twice and then let it end: turns %d objections %d", turns, objections)
	}
	exec.mu.Lock()
	first := exec.objections[0]
	exec.mu.Unlock()
	if first.Source != "gator" || !strings.Contains(first.Reason, "committed nothing") {
		t.Fatalf("objection: %+v", first)
	}
	if got := (*done.Receipt)["objections"]; got != float64(2) {
		t.Fatalf("receipt: %v", *done.Receipt)
	}
}
