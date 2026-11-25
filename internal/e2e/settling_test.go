package e2e

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
)

// A job's card follows its start as the runner tells it (checkout, then ready), and once the
// agent is done the job is settling — still holding its lease, its card finishing — until the
// receipt makes it done. The task's jobs can be asked by kind.
func TestAJobSettlesBeforeItIsDone(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	job := h.job(task, backend, "ok", 0)
	raw, _ := h.dialRaw(h.runner, backend, proto.Version, 1)
	raw.lease(1)
	id := job.Id.String()
	event := func(seq uint64, typ string, payload any) {
		t.Helper()
		b, _ := json.Marshal(payload)
		raw.send(proto.TypeEvents, proto.Events{JobID: id, Events: []proto.JobEvent{{Seq: seq, Type: typ, At: time.Now(), Payload: b}}})
		raw.recvType(proto.TypeAck)
	}
	jobCard := func(ok func(gen.Message) bool) {
		t.Helper()
		h.threadCard(task, "job", ok)
	}
	event(1, "startup_phase", map[string]string{"phase": "checkout"})
	jobCard(func(m gen.Message) bool { return m.Card != nil && m.Card.AdditionalProperties["startup"] == "checkout" })
	event(2, "startup_phase", map[string]string{"phase": "ready"})
	jobCard(func(m gen.Message) bool { return m.Card != nil && m.Card.AdditionalProperties["startup"] == "ready" })

	event(3, "settling", map[string]string{"status": "done"})
	if j := h.waitJob(id, "settling"); j.Status != "settling" {
		t.Fatalf("job: %+v", j)
	}
	jobCard(func(m gen.Message) bool { return m.Card != nil && deref(m.Card.State) == "settling" })
	// Still the runner's: a heartbeat keeps its lease, and an event does not make it run again.
	raw.send(proto.TypeHeartbeat, proto.Heartbeat{ActiveJobs: []string{id}})
	event(4, "git", map[string]int{"commits": 0})
	if j := h.waitJob(id, "settling"); j.Status != "settling" {
		t.Fatalf("an event after settling: %+v", j)
	}
	raw.send(proto.TypeFinish, proto.Finish{JobID: id, Status: proto.StatusDone, Usage: proto.Usage{DurationMS: 10, InputTokens: 1}})
	raw.recvType(proto.TypeAck)
	h.waitJob(id, "done")

	var phase, reply []gen.Job
	h.do("GET", "/tasks/"+task.Id.String()+"/jobs?kind=phase", nil, &phase)
	h.do("GET", "/tasks/"+task.Id.String()+"/jobs?kind=reply", nil, &reply)
	if len(phase) != 1 || len(reply) != 0 {
		t.Fatalf("by kind: %d phase, %d reply", len(phase), len(reply))
	}
}
