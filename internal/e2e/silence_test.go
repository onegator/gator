package e2e

import (
	"context"
	"fmt"
	"log/slog"
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
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go notify.New(h.pool, h.svc, sender, h.hub, slog.Default(), 50*time.Millisecond).Run(ctx)
	time.Sleep(150 * time.Millisecond) // let the cursor start at the newest event

	// The development database is shared, so older runners are already silent. Sweep them
	// away first and count what that produced; this test only judges what comes after.
	if _, err := h.mgr.Sweep(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before := settled(sender)

	// A plain disconnect is not news: the runner usually comes straight back.
	raw, _ := h.dialRaw(h.runner, uniqueBackend(), proto.Version, 1)
	raw.c.CloseNow()
	if _, err := h.mgr.Sweep(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := settled(sender); got != before {
		t.Fatalf("a disconnect woke someone: %d new messages", got-before)
	}

	// Silence past OfflineAfter is different: the session is still there, the runner is not.
	live, _ := h.dialRaw(h.runner, uniqueBackend(), proto.Version, 1)
	defer live.c.CloseNow()
	if _, err := h.mgr.Sweep(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var got notify.Message
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && got.Link == "" {
		for _, m := range sender.messages() {
			if m.Link == "gator://runners" {
				got = m
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.Link == "" {
		t.Fatal("a runner that stopped answering should reach the admins")
	}
	if got.Title == "" || got.Body == "" {
		t.Fatalf("the message should say what happened: %+v", got)
	}

	// The runner list says since when and why, so a person can tell a blip from an outage.
	var list []gen.RunnerInfo
	if code := h.do("GET", "/runners", nil, &list); code != 200 {
		t.Fatalf("runners: %d", code)
	}
	var silent, dropped *gen.RunnerInfo
	for i := range list {
		switch {
		case list[i].Status != gen.Offline || list[i].OfflineReason == nil:
		case *list[i].OfflineReason == "no heartbeat":
			silent = &list[i]
		case *list[i].OfflineReason == "disconnected":
			dropped = &list[i]
		}
	}
	if silent == nil || silent.OfflineSince == nil {
		t.Fatalf("a silent runner should say since when: %+v", silent)
	}
	if dropped == nil || dropped.OfflineSince == nil {
		t.Fatalf("a disconnected runner should say so too: %+v", dropped)
	}
}

// settled waits for the notifier to go quiet and returns the count it settled on. Counting
// after a fixed sleep raced the delivery of the previous sweep's messages.
func settled(s *fakeSender) int {
	last, stableSince := -1, time.Now()
	for time.Since(stableSince) < 600*time.Millisecond {
		if n := runnerMessages(s); n != last {
			last, stableSince = n, time.Now()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

// runnerMessages counts the pushes about runners the fake sender has seen.
func runnerMessages(s *fakeSender) int {
	n := 0
	for _, m := range s.messages() {
		if m.Link == "gator://runners" {
			n++
		}
	}
	return n
}

// The reason enum is part of the contract the app switches on.
func TestNoRunnerReasonIsInTheAPI(t *testing.T) {
	if string(gen.DecisionReasonNoRunner) != string(process.ReasonNoRunner) {
		t.Fatalf("api %q and process %q disagree", gen.DecisionReasonNoRunner, process.ReasonNoRunner)
	}
}
