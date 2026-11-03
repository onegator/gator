package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/notify"
	"github.com/onegator/gator/internal/server/process"
)

// unassignableAfter matches runners.Config's default; the harness does not override it.
const unassignableAfter = 2 * time.Minute

// A job whose backend no runner offers used to wait in silence: the inbox hides a task while
// its job is queued, because an agent is supposedly on it. Nobody was.
func TestQueuedJobWithNoRunnerReachesTheInbox(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend() // no runner in this test offers it
	task := h.task()
	job := h.job(task, backend, "ok", 0)

	// Before the grace period the queue is simply young, and silence is correct.
	if _, err := h.mgr.Sweep(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if d := has(h.decisions(""), task); d != nil && d.Reason == gen.DecisionReasonNoRunner {
		t.Fatal("a job queued a moment ago is not yet a problem")
	}

	if _, err := h.mgr.Sweep(context.Background(), time.Now().Add(unassignableAfter+time.Second)); err != nil {
		t.Fatal(err)
	}
	d := has(h.decisions(""), task)
	if d == nil || d.Reason != gen.DecisionReasonNoRunner {
		t.Fatalf("a job nobody can take belongs in the inbox: %+v", d)
	}

	// A runner that can take it clears the flag as it leases, and the row leaves the inbox.
	h.startRunner(backend, fakeExec{})
	if j := h.waitJob(job.Id.String(), "done"); j.Status != "done" {
		t.Fatalf("job: %+v", j)
	}
	if d := has(h.decisions(""), task); d != nil && d.Reason == gen.DecisionReasonNoRunner {
		t.Fatal("the job found a runner; the inbox should stop asking")
	}
}

// A runner that is online but whose backend login expired takes nothing. That is the same
// silence as having no runner at all, so it is reported the same way.
func TestRunnerWithExpiredLoginCountsAsNoRunner(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	h.job(task, backend, "ok", 0)

	raw, reply := h.dialRaw(h.runner, backend, proto.Version, 1)
	if reply.Type != proto.TypeRegistered {
		t.Fatalf("register: %s", reply.Type)
	}
	raw.send(proto.TypeHeartbeat, proto.Heartbeat{AuthState: map[string]string{backend: "expired"}})
	time.Sleep(200 * time.Millisecond) // let the server store the auth state

	if _, err := h.mgr.Sweep(context.Background(), time.Now().Add(unassignableAfter+time.Second)); err != nil {
		t.Fatal(err)
	}
	d := has(h.decisions(""), task)
	if d == nil || d.Reason != gen.DecisionReasonNoRunner {
		t.Fatalf("an expired login leaves the job unassignable: %+v", d)
	}
}

// Losing a runner is worth waking someone for: until it is back, queued work waits.
func TestSilentRunnerNotifiesTheAdmins(t *testing.T) {
	h := newHarness(t)
	if code := h.do("POST", "/devices", gen.NewDevice{Token: fmt.Sprintf("apns-%d", time.Now().UnixNano()), Platform: gen.Macos}, nil); code != 200 {
		t.Fatalf("register device: %d", code)
	}
	sender := &fakeSender{}
	ctx := t.Context()
	go notify.New(h.pool, h.svc, sender, h.hub, slog.Default(), 50*time.Millisecond).Run(ctx)
	time.Sleep(150 * time.Millisecond) // let the cursor start at the newest event

	// The database is shared with other tests and packages whose runners go quiet too, so
	// only the messages about this test's runners count; each dial carries its own name.

	// A plain disconnect is not news: the runner usually comes straight back. The runner list
	// still says why and since when, so a person can tell a blip from an outage.
	backend := uniqueBackend()
	raw, reg := h.dialRaw(h.runner, backend, proto.Version, 1)
	id := registeredID(t, reg)
	raw.c.CloseNow()
	dropped := h.waitRunner(id, func(r gen.RunnerInfo) bool { return r.Status == gen.Offline })
	if dropped.OfflineReason == nil || *dropped.OfflineReason != "disconnected" || dropped.OfflineSince == nil {
		t.Fatalf("a disconnected runner should say so: %+v", dropped)
	}
	if _, err := h.mgr.Sweep(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := settled(sender, "raw-"+backend); n != 0 {
		t.Fatalf("a disconnect woke someone: %d messages", n)
	}

	// Silence past OfflineAfter is different: the session is still there, the runner is not.
	backend = uniqueBackend()
	live, reg := h.dialRaw(h.runner, backend, proto.Version, 1)
	defer live.c.CloseNow()
	if registeredID(t, reg) != id {
		t.Fatal("the same token reconnects as the same runner")
	}
	if _, err := h.mgr.Sweep(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := settled(sender, "raw-"+backend); n == 0 {
		t.Fatal("a runner that stopped answering should reach the admins")
	}
	for _, m := range runnerMessages(sender, "raw-"+backend) {
		if m.Body == "" {
			t.Fatalf("the message should say what happened: %+v", m)
		}
	}

	// Closing its session for the silence does not turn the reason into a disconnect.
	silent := h.waitRunner(id, func(r gen.RunnerInfo) bool { return !r.Connected })
	if silent.Status != gen.Offline || silent.OfflineReason == nil || *silent.OfflineReason != "no heartbeat" || silent.OfflineSince == nil {
		t.Fatalf("a silent runner should say so, and since when: %+v", silent)
	}
}

// registeredID is the runner id in the server's answer to a register.
func registeredID(t *testing.T, reply proto.RawEnvelope) string {
	t.Helper()
	if reply.Type != proto.TypeRegistered {
		t.Fatalf("register: %s", reply.Type)
	}
	var r proto.Registered
	if err := json.Unmarshal(reply.Payload, &r); err != nil {
		t.Fatal(err)
	}
	return r.RunnerID
}

// waitRunner polls the runner list until the runner with id satisfies ok.
func (h *harness) waitRunner(id string, ok func(gen.RunnerInfo) bool) gen.RunnerInfo {
	h.t.Helper()
	var last gen.RunnerInfo
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var list []gen.RunnerInfo
		if code := h.do("GET", "/runners", nil, &list); code != 200 {
			h.t.Fatalf("runners: %d", code)
		}
		for _, r := range list {
			if r.Id.String() == id {
				if last = r; ok(r) {
					return r
				}
			}
		}
	}
	h.t.Fatalf("runner %s never got there: %+v", id, last)
	return last
}

// settled waits for the notifier to go quiet about the named runner and returns how many
// messages it settled on. Counting after a fixed sleep raced their delivery.
func settled(s *fakeSender, name string) int {
	last, stableSince := -1, time.Now()
	for time.Since(stableSince) < time.Second {
		if n := len(runnerMessages(s, name)); n != last {
			last, stableSince = n, time.Now()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

// runnerMessages are the pushes the fake sender has seen about the named runner.
func runnerMessages(s *fakeSender, name string) []notify.Message {
	var ms []notify.Message
	for _, m := range s.messages() {
		if m.Link == "gator://runners" && strings.Contains(m.Title, " "+name+" ") {
			ms = append(ms, m)
		}
	}
	return ms
}

// The reason enum is part of the contract the app switches on.
func TestNoRunnerReasonIsInTheAPI(t *testing.T) {
	if string(gen.DecisionReasonNoRunner) != string(process.ReasonNoRunner) {
		t.Fatalf("api %q and process %q disagree", gen.DecisionReasonNoRunner, process.ReasonNoRunner)
	}
}
