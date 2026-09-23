package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// A runner that is finished with used to stay on the list for ever, and — worse — its token
// stayed valid, because nothing ever revoked it. Forgetting one has to do both halves: take it
// off the list and lock it out. What it must not do is lose the record of its work.

func runnerNamed(h *harness, list []gen.RunnerInfo, id string) (gen.RunnerInfo, bool) {
	h.t.Helper()
	for _, r := range list {
		if r.Id.String() == id {
			return r, true
		}
	}
	return gen.RunnerInfo{}, false
}

func (h *harness) runners() []gen.RunnerInfo {
	h.t.Helper()
	var out []gen.RunnerInfo
	h.do("GET", "/runners", nil, &out)
	return out
}

func TestForgettingARunnerLocksItOutAndKeepsItsWork(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	_, stop := h.startRunner(backend, fakeExec{})

	job := h.job(h.task(), backend, "ok", 0)
	done := h.waitJob(job.Id.String(), "done", "failed")
	if done.Status != "done" || done.RunnerId == nil {
		t.Fatalf("job: %+v", done)
	}
	id := done.RunnerId.String()

	// While it is connected, forgetting it would achieve nothing: it comes back on its next
	// heartbeat, as a runner that is running.
	if code := h.do("DELETE", "/runners/"+id, nil, nil); code != http.StatusConflict {
		t.Fatalf("a connected runner must be refused, got %d", code)
	}
	stop()
	h.until("the runner to disconnect", func() bool {
		r, ok := runnerNamed(h, h.runners(), id)
		return ok && !r.Connected
	})

	if code := h.do("DELETE", "/runners/"+id, nil, nil); code != http.StatusNoContent {
		t.Fatalf("forget: %d", code)
	}
	if _, ok := runnerNamed(h, h.runners(), id); ok {
		t.Fatal("a forgotten runner should be off the list")
	}
	// Saying it twice is the same answer, not an error: forgetting is a state, not an event.
	if code := h.do("DELETE", "/runners/"+id, nil, nil); code != http.StatusNoContent {
		t.Fatalf("forgetting twice: %d", code)
	}

	// The token stops authenticating. This is the half that was missing before: disabling a
	// runner left its credentials working.
	if n := h.count("SELECT count(*) FROM api_tokens t JOIN runners r ON r.token_id = t.id WHERE r.id = $1 AND t.revoked_at IS NULL", id); n != 0 {
		t.Fatalf("the runner's token should be revoked, %d still valid", n)
	}

	// And the work it did still knows who did it.
	var after gen.Job
	h.do("GET", "/jobs/"+job.Id.String(), nil, &after)
	if after.RunnerId == nil || after.RunnerId.String() != id {
		t.Fatalf("a forgotten runner must not erase its own history: %+v", after.RunnerId)
	}
	if n := h.count("SELECT count(*) FROM runners WHERE id = $1 AND retired_at IS NOT NULL", id); n != 1 {
		t.Fatalf("the row should stay, marked forgotten: %d", n)
	}
}

// Asking a runner to sign a backend in used to answer "accepted" and then do nothing at all:
// the message reached the runner, which wrote one line into a log file and stopped. It is
// refused now, where a person can read the refusal.
func TestARunnerThatCannotSignInSaysSoInsteadOfAccepting(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	_, stop := h.startRunner(backend, fakeExec{})
	defer stop()

	var id string
	h.until("the runner to register", func() bool {
		for _, r := range h.runners() {
			if r.Connected && slices.Contains(r.Capabilities.Backends, backend) {
				id = r.Id.String()
				return true
			}
		}
		return false
	})
	for _, r := range h.runners() {
		if r.Id.String() == id && r.Capabilities.CanLogin != nil && *r.Capabilities.CanLogin {
			t.Fatal("no runner can drive a login today; saying it can would be the old lie in a new place")
		}
	}
	var problem gen.Error
	if code := h.do("POST", "/runners/"+id+"/login", gen.LoginRequest{Backend: "claude"}, &problem); code != http.StatusConflict {
		t.Fatalf("login request: %d", code)
	}
	if !strings.Contains(problem.Error, "on that machine") {
		t.Fatalf("the refusal must say what to do instead: %q", problem.Error)
	}
}
