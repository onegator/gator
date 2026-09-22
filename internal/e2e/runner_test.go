package e2e

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/server/api/gen"
)

// fakeExec behaves according to the job instruction.
type fakeExec struct{}

func (fakeExec) Run(ctx context.Context, job proto.Job, io client.JobIO) proto.Finish {
	usage := proto.Usage{Backend: job.Backend, Model: "fake-1", InputTokens: 100, OutputTokens: 20, DurationMS: 1500, CostUSD: 0.01, CostEstimated: true}
	switch job.Instruction {
	case "ok":
		io.Emit("text", map[string]string{"text": "hello"})
		io.Emit("tool_call", map[string]string{"name": "bash"})
		return proto.Finish{Status: proto.StatusDone, Summary: "did it", Usage: usage}
	case "nousage":
		io.Emit("text", map[string]string{"text": "done, but no numbers"})
		return proto.Finish{Status: proto.StatusDone}
	case "wait":
		io.Emit("text", map[string]string{"text": "started"})
		for {
			select {
			case m := <-io.Steer:
				io.Emit("steered", map[string]string{"message": m})
			case <-ctx.Done():
				return proto.Finish{Status: proto.StatusStopped, StopReason: context.Cause(ctx).Error(), Usage: proto.Usage{DurationMS: 10}}
			}
		}
	}
	return proto.Finish{Status: proto.StatusFailed, StopReason: "unknown instruction"}
}

func TestRunnerRunsJobEndToEnd(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	_, stop := h.startRunner(backend, fakeExec{})

	job := h.job(task, backend, "ok", 0)
	if job.Role != "planner" || job.Phase != "planning" {
		t.Fatalf("job should take the phase's runner role: %s/%s", job.Phase, job.Role)
	}
	done := h.waitJob(job.Id.String(), "done", "failed")
	if done.Status != "done" || done.Receipt == nil || done.StartedAt == nil || done.FinishedAt == nil || done.Attempts != 1 {
		t.Fatalf("finished job: %+v reason=%s", done, deref(done.StopReason))
	}
	if (*done.Receipt)["summary"] != "did it" {
		t.Fatalf("receipt summary: %v", *done.Receipt)
	}

	var evs []gen.JobEvent
	h.do("GET", "/jobs/"+job.Id.String()+"/events", nil, &evs)
	if len(evs) != 2 || evs[0].Type != "text" || evs[1].Seq != 2 || evs[0].Payload["text"] != "hello" {
		t.Fatalf("events: %+v", evs)
	}
	var m gen.TaskMetrics
	h.do("GET", "/tasks/"+task.Id.String()+"/metrics", nil, &m)
	if m.Tokens.Total != 120 || m.Jobs != 1 || m.AgentSeconds != 1.5 {
		t.Fatalf("usage from the receipt should reach task metrics: %+v", m)
	}
	// After the job the task waits for a person; the lead splits into its parts.
	if m.State.Kind != "waiting_for_human" || m.WorkSeconds <= 0 || done.FinishedAt == nil || m.State.Since.Before(done.FinishedAt.Add(-time.Second)) {
		t.Fatalf("state after the job: %+v work=%v", m.State, m.WorkSeconds)
	}
	if sum := m.WorkSeconds + m.QueueSeconds + m.BlockedSeconds + m.WaitingSeconds; sum < m.LeadSeconds-0.01 || sum > m.LeadSeconds+0.01 {
		t.Fatalf("parts %v do not add up to lead %v", sum, m.LeadSeconds)
	}

	var rs []gen.RunnerInfo
	h.do("GET", "/runners", nil, &rs)
	found := false
	for _, r := range rs {
		if r.Name == "e2e" && r.Connected && len(r.Capabilities.Backends) == 1 && r.Capabilities.Backends[0] == backend {
			found = true
		}
	}
	if !found {
		t.Fatalf("connected runner not listed: %+v", rs)
	}

	stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.do("GET", "/runners", nil, &rs)
		still := false
		for _, r := range rs {
			if r.Capabilities.Backends != nil && len(r.Capabilities.Backends) == 1 && r.Capabilities.Backends[0] == backend && (r.Connected || r.Status != "offline") {
				still = true
			}
		}
		if !still {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("runner still online after it stopped")
}

func TestSteerAndStop(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	h.startRunner(backend, fakeExec{})

	job := h.job(task, backend, "wait", 0)
	h.waitJob(job.Id.String(), "running")
	// The runner list says what each runner is working on, which is how a person on a phone —
	// with no task detail open — finds the job to steer or stop.
	var runners []gen.RunnerInfo
	h.do("GET", "/runners", nil, &runners)
	found := false
	for _, rn := range runners {
		if rn.Jobs == nil {
			continue
		}
		for _, j := range *rn.Jobs {
			if j.Id == job.Id {
				found = j.TaskTitle == task.Title && j.Status == "running"
			}
		}
	}
	if !found {
		t.Fatalf("the running job should be listed under its runner: %+v", runners)
	}
	if code := h.do("POST", "/jobs/"+job.Id.String()+"/steer", gen.SteerJob{Message: "use the smaller fix"}, nil); code != 202 {
		t.Fatalf("steer: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var evs []gen.JobEvent
		h.do("GET", "/jobs/"+job.Id.String()+"/events", nil, &evs)
		if len(evs) >= 2 && evs[1].Type == "steered" && evs[1].Payload["message"] == "use the smaller fix" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("steer never reached the executor: %+v", evs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	reason := "wrong approach"
	if code := h.do("POST", "/jobs/"+job.Id.String()+"/stop", gen.StopJob{Reason: &reason}, nil); code != 202 {
		t.Fatalf("stop: %d", code)
	}
	j := h.waitJob(job.Id.String(), "stopped")
	if !strings.Contains(deref(j.StopReason), "wrong approach") {
		t.Fatalf("stop reason: %q", deref(j.StopReason))
	}
	if code := h.do("POST", "/jobs/"+job.Id.String()+"/stop", nil, nil); code != 409 {
		t.Fatalf("stopping a finished job: %d", code)
	}
}

func TestDoneWithoutUsageIsFailed(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	h.startRunner(backend, fakeExec{})
	job := h.job(task, backend, "nousage", 0)
	j := h.waitJob(job.Id.String(), "done", "failed")
	if j.Status != "failed" || deref(j.StopReason) != "done without a usage report" {
		t.Fatalf("done without usage must fail: %s %q", j.Status, deref(j.StopReason))
	}
}

func TestStopQueuedJobIsImmediate(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	job := h.job(task, uniqueBackend(), "ok", 0) // nobody serves this backend
	if code := h.do("POST", "/jobs/"+job.Id.String()+"/stop", nil, nil); code != 202 {
		t.Fatalf("stop queued: %d", code)
	}
	if j := h.waitJob(job.Id.String(), "stopped"); j.RunnerId != nil {
		t.Fatalf("queued job should stop without a runner: %+v", j)
	}
}

func TestHandshakeRejections(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := strings.Replace(h.srv.URL, "http", "ws", 1) + proto.Path
	_, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + h.admin}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a user token must not open a runner session: err=%v resp=%v", err, resp)
	}
	_, resp, err = websocket.Dial(ctx, u, nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous runner session: err=%v", err)
	}

	_, reply := h.dialRaw(h.runner, uniqueBackend(), proto.MinSupportedVersion-1, 1)
	var e proto.Error
	if reply.Type != proto.TypeError || reply.Into(&e) != nil || e.Code != "unsupported_version" || !e.Fatal {
		t.Fatalf("old protocol must be refused: %+v %+v", reply, e)
	}
}

func TestLostLeaseGoesBackToQueueAndAnotherRunnerFinishes(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	job := h.job(task, backend, "ok", 0)

	raw, reply := h.dialRaw(h.runner, backend, proto.Version, 1)
	if reply.Type != proto.TypeRegistered {
		t.Fatalf("register: %s", reply.Type)
	}
	jobs := raw.lease(1)
	if len(jobs) != 1 || jobs[0].JobID != job.Id.String() || jobs[0].Attempt != 1 {
		t.Fatalf("lease: %+v", jobs)
	}
	raw.c.CloseNow() // the runner vanishes without a heartbeat or a finish

	if _, err := h.mgr.Sweep(context.Background(), time.Now().Add(testLeaseTTL+time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := h.waitJob(job.Id.String(), "queued"); j.RunnerId != nil || j.Attempts != 1 {
		t.Fatalf("expired lease should requeue: %+v", j)
	}
	h.startRunner(backend, fakeExec{})
	if j := h.waitJob(job.Id.String(), "done"); j.Attempts != 2 {
		t.Fatalf("second runner should finish on attempt 2: %+v", j)
	}
}

func TestLeaseExpiryFailsAfterMaxAttempts(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	job := h.job(h.task(), backend, "ok", 1)
	raw, _ := h.dialRaw(h.runner, backend, proto.Version, 1)
	if got := raw.lease(1); len(got) != 1 {
		t.Fatalf("lease: %+v", got)
	}
	raw.c.CloseNow()
	if _, err := h.mgr.Sweep(context.Background(), time.Now().Add(testLeaseTTL+time.Second)); err != nil {
		t.Fatal(err)
	}
	j := h.waitJob(job.Id.String(), "failed")
	if deref(j.StopReason) != "lease expired after 1 attempts" {
		t.Fatalf("reason: %q", deref(j.StopReason))
	}
}

func TestStalledJobResumesOnNextEvent(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	job := h.job(h.task(), backend, "ok", 0)
	raw, _ := h.dialRaw(h.runner, backend, proto.Version, 1)
	if got := raw.lease(1); len(got) != 1 {
		t.Fatalf("lease: %+v", got)
	}
	raw.send(proto.TypeEvents, proto.Events{JobID: job.Id.String(), Events: []proto.JobEvent{{Seq: 1, Type: "text", At: time.Now()}}})
	raw.recvType(proto.TypeAck)
	h.waitJob(job.Id.String(), "running")

	if _, err := h.mgr.Sweep(context.Background(), time.Now().Add(testStallAfter+time.Second)); err != nil {
		t.Fatal(err)
	}
	h.waitJob(job.Id.String(), "stalled")

	// a resend of event 1 is ignored; event 2 revives the job
	raw.send(proto.TypeEvents, proto.Events{JobID: job.Id.String(), Events: []proto.JobEvent{{Seq: 1, Type: "text", At: time.Now()}, {Seq: 2, Type: "text", At: time.Now()}}})
	raw.recvType(proto.TypeAck)
	h.waitJob(job.Id.String(), "running")
	var evs []gen.JobEvent
	h.do("GET", "/jobs/"+job.Id.String()+"/events", nil, &evs)
	if len(evs) != 2 {
		t.Fatalf("resent event stored twice: %d events", len(evs))
	}
}

func TestConcurrentRunnersLeaseDisjointJobs(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	want := map[string]bool{}
	for i := 0; i < 6; i++ {
		want[h.job(task, backend, "ok", 0).Id.String()] = true
	}
	a, _ := h.dialRaw(h.runner, backend, proto.Version, 6)
	other, err := runnerToken(h)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := h.dialRaw(other, backend, proto.Version, 6)

	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for _, r := range []*rawRunner{a, b} {
		wg.Add(1)
		go func(r *rawRunner) {
			defer wg.Done()
			for _, j := range r.lease(6) {
				mu.Lock()
				got[j.JobID]++
				mu.Unlock()
			}
		}(r)
	}
	wg.Wait()
	if len(got) != len(want) {
		t.Fatalf("leased %d distinct jobs, want %d", len(got), len(want))
	}
	for id, n := range got {
		if !want[id] || n != 1 {
			t.Fatalf("job %s leased %d times (known=%v)", id, n, want[id])
		}
	}
}
