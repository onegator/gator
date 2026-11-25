package e2e

import (
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
)

// What a runner says each backend can do reaches people twice: on the runner, and on every job
// it took, as the capabilities of that job's backend. A heartbeat that says a backend can now do
// less (it fell back to print mode) changes both.
func TestBackendCapabilitiesReachTheRunnerAndItsJobs(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	job := h.job(h.task(), backend, "ok", 0)
	live := proto.BackendCapabilities{ActiveSteering: true, NativeInterrupt: true, ToolApprovals: true, ApprovalsLiveOnly: true,
		Attachments: true, Checkpoints: true, Enforcement: proto.EnforcementNative}
	reg := proto.Register{Name: "caps", Location: "other", Capabilities: proto.Capabilities{Backends: []string{backend}, MaxParallel: 1,
		PerBackend: map[string]proto.BackendCapabilities{backend: live}}}
	raw, reply := h.dialRegistered(h.runner, reg, proto.Version)
	var r proto.Registered
	if reply.Type != proto.TypeRegistered || reply.Into(&r) != nil {
		t.Fatalf("register: %+v", reply)
	}
	if got := raw.lease(1); len(got) != 1 || got[0].JobID != job.Id.String() {
		t.Fatalf("lease: %+v", got)
	}

	var info gen.RunnerInfo
	if code := h.do("GET", "/runners/"+r.RunnerID, nil, &info); code != 200 {
		t.Fatalf("get runner: %d", code)
	}
	got := (*info.Capabilities.PerBackend)[backend]
	if !got.ActiveSteering || !got.ToolApprovals || !got.Checkpoints || got.Enforcement != gen.BackendCapabilitiesEnforcementNative {
		t.Fatalf("runner capabilities: %+v", got)
	}
	var j gen.Job
	h.do("GET", "/jobs/"+job.Id.String(), nil, &j)
	if j.Capabilities == nil || !j.Capabilities.ActiveSteering || !j.Capabilities.NativeInterrupt {
		t.Fatalf("job capabilities: %+v", j.Capabilities)
	}

	raw.send(proto.TypeHeartbeat, proto.Heartbeat{AuthState: map[string]string{backend: "ok"}, ActiveJobs: []string{job.Id.String()},
		Capabilities: map[string]proto.BackendCapabilities{backend: {Enforcement: proto.EnforcementNative}}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.do("GET", "/jobs/"+job.Id.String(), nil, &j)
		if j.Capabilities != nil && !j.Capabilities.ActiveSteering {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fallback did not reach the job: %+v", j.Capabilities)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var list []gen.Job
	h.do("GET", "/tasks/"+job.TaskId.String()+"/jobs", nil, &list)
	if len(list) != 1 || list[0].Capabilities == nil || list[0].Capabilities.ToolApprovals {
		t.Fatalf("task's jobs: %+v", list)
	}
	if code := h.do("GET", "/runners/00000000-0000-4000-8000-000000000000", nil, nil); code != 404 {
		t.Fatalf("unknown runner: %d", code)
	}
}
