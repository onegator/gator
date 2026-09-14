package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/server/api/gen"
)

// digestExec returns a digest the way a real session would after the executor extracted it.
type digestExec struct{ digest *proto.Digest }

func (d digestExec) Run(_ context.Context, job proto.Job, io client.JobIO) proto.Finish {
	io.Emit("text", map[string]string{"text": "working"})
	return proto.Finish{Status: proto.StatusDone, Summary: "## Report\nok", Digest: d.digest, Usage: proto.Usage{Backend: job.Backend, DurationMS: 10}}
}

// Work done on one backend reaches a job on another through the working state.
func TestWorkingStateCarriesAcrossBackends(t *testing.T) {
	h := newHarness(t)
	first, second := uniqueBackend(), uniqueBackend()
	task := h.task() // bug, planning (planner)

	h.startRunner(first, digestExec{digest: &proto.Digest{
		Changes: []string{"mapped the crash to the session cache"}, Decisions: []string{"fix the cache, not the caller"},
		Rejected: []string{"retrying the request"}, Left: []string{"write the regression test"}}})
	j1 := h.job(task, first, "plan it", 0)
	h.waitJob(j1.Id.String(), "done")

	var arts []gen.Artifact
	h.do("GET", "/tasks/"+task.Id.String()+"/artifacts", nil, &arts)
	var ws *gen.Artifact
	for i := range arts {
		if arts[i].Type == "working_state" {
			ws = &arts[i]
		}
	}
	if ws == nil || ws.Phase != "task" || ws.Content == nil {
		t.Fatalf("no working state after a done job: %+v", arts)
	}
	for _, want := range []string{"## Done so far", "mapped the crash to the session cache", "## Decisions", "fix the cache, not the caller", "## Rejected approaches", "retrying the request", "## What is left", "write the regression test"} {
		if !strings.Contains(*ws.Content, want) {
			t.Fatalf("working state lacks %q:\n%s", want, *ws.Content)
		}
	}
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE type = 'job.digest' AND aggregate_id = $1`, j1.Id.String()).Scan(&n); err != nil || n != 1 {
		t.Fatalf("job.digest events: %d %v", n, err)
	}

	// A different backend picks the task up and receives the working state first.
	other, err := runnerToken(h)
	if err != nil {
		t.Fatal(err)
	}
	h.runner = other
	rec := &recordingExec{}
	h.startRunner(second, rec)
	j2 := h.job(task, second, "continue", 0)
	h.waitJob(j2.Id.String(), "done")
	got := rec.last()
	if len(got.Context) == 0 || got.Context[0].Kind != "working_state" || !strings.Contains(got.Context[0].Body, "fix the cache, not the caller") {
		t.Fatalf("the next job should start from the working state: %+v", got.Context)
	}

	// That job left no digest: the event says so and the working state admits it.
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE type = 'job.digest_missing' AND aggregate_id = $1`, j2.Id.String()).Scan(&n); err != nil || n != 1 {
		t.Fatalf("job.digest_missing events: %d %v", n, err)
	}
	h.do("GET", "/tasks/"+task.Id.String()+"/artifacts", nil, &arts)
	latest := ""
	for _, a := range arts {
		if a.Type == "working_state" && a.Content != nil {
			latest = *a.Content
		}
	}
	if !strings.Contains(latest, "The last job left no digest") || !strings.Contains(latest, "fix the cache, not the caller") {
		t.Fatalf("latest working state:\n%s", latest)
	}
}
