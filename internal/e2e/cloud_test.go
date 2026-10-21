package e2e

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/runners"
)

// inProcessCloud is a provisioner whose machines are gator-runner clients in this process,
// started from exactly the environment a real machine would get.
type inProcessCloud struct {
	h        *harness
	mu       sync.Mutex
	tokens   map[string]string // job id → the runner token its machine got
	exited   chan string
	stopped  []string
	machines map[string]context.CancelFunc
}

func (p *inProcessCloud) Name() string { return "in-process" }

func (p *inProcessCloud) Start(_ context.Context, spec runners.MachineSpec) (string, error) {
	env := spec.Env
	p.mu.Lock()
	p.tokens[spec.JobID] = env["GATOR_RUNNER_TOKEN"]
	p.mu.Unlock()
	c := client.New(client.Config{ServerURL: p.h.srv.URL, Token: env["GATOR_RUNNER_TOKEN"], Name: env["GATOR_RUNNER_NAME"],
		Location: env["GATOR_RUNNER_LOCATION"], Placement: env["GATOR_RUNNER_PLACEMENT"], Once: env["GATOR_RUNNER_ONCE"] == "1",
		Backends: []string{env["GATOR_RUNNER_BACKENDS"]}, Projects: []string{env["GATOR_RUNNER_PROJECTS"]},
		HeartbeatInterval: 100 * time.Millisecond, MinBackoff: 50 * time.Millisecond}, fakeExec{}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.machines[spec.Name] = cancel
	p.mu.Unlock()
	go func() {
		_ = c.Run(ctx)
		p.exited <- spec.Name
	}()
	return spec.Name, nil
}

func (p *inProcessCloud) Stop(_ context.Context, handle string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = append(p.stopped, handle)
	if cancel := p.machines[handle]; cancel != nil {
		cancel()
	}
	return nil
}

// Run in the cloud: the job waits for no Mac, a runner starts for it, does it and goes away,
// while the Mac that is connected the whole time never touches it.
func TestCloudJobRunsOnARunnerStartedForIt(t *testing.T) {
	// Other tests' cloud jobs may sit in the same database; this one only counts its own.
	cloud := &inProcessCloud{exited: make(chan string, 16), machines: map[string]context.CancelFunc{}, tokens: map[string]string{}}
	h := newHarness(t, func(c *runners.Config) {
		c.Provisioner = cloud
		c.CloudServerURL = "http://overridden-by-the-fake"
		c.ProvisionDelay = 50 * time.Millisecond
	})
	cloud.h = h
	backend := uniqueBackend()
	task := h.task()
	mac, _ := h.startRunner(backend, fakeExec{})

	instruction, placement := "ok", gen.NewJobPlacementCloud
	var job gen.Job
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/jobs", gen.NewJob{Backend: &backend, Instruction: &instruction, Placement: &placement}, &job); code != 201 {
		t.Fatalf("create: %d", code)
	}
	if job.Placement != gen.JobPlacementCloud {
		t.Fatalf("placement %q", job.Placement)
	}
	done := h.waitJob(job.Id.String(), "done", "failed")
	if done.Status != "done" {
		t.Fatalf("job %s: %v", done.Status, deref(done.StopReason))
	}
	cloud.mu.Lock()
	token := cloud.tokens[job.Id.String()]
	cloud.mu.Unlock()
	if token == "" {
		t.Fatal("no machine was started for the cloud job")
	}
	wantMachine := "gator-job-" + strings.ReplaceAll(job.Id.String(), "-", "")[:12]
	for exited := false; !exited; {
		select {
		case name := <-cloud.exited:
			exited = name == wantMachine
		case <-time.After(10 * time.Second):
			t.Fatal("the cloud runner did not exit after its job")
		}
	}
	_ = mac

	var rs []gen.RunnerInfo
	h.do("GET", "/runners", nil, &rs)
	for _, r := range rs {
		if done.RunnerId != nil && r.Id == *done.RunnerId && r.Placement != gen.Cloud {
			t.Fatalf("the job ran on %s, a %s runner", r.Name, r.Placement)
		}
	}

	// Its token leased that job and nothing else; it reads no project and dies with the job.
	if code := h.asJob(token, "GET", "/tasks/"+task.Id.String(), nil, nil); code != 403 && code != 401 {
		t.Fatalf("a one-job runner token read a task: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if code := h.asJob(token, "GET", "/projects", nil, nil); code == 401 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the one-job runner token outlived its job")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// And the local job goes to the Mac, not to a machine.
	local := h.job(task, backend, "ok", 0)
	if local.Placement != gen.JobPlacementLocal {
		t.Fatalf("default placement %q", local.Placement)
	}
	if j := h.waitJob(local.Id.String(), "done", "failed"); j.Status != "done" {
		t.Fatalf("local job %s", j.Status)
	}
	cloud.mu.Lock()
	_, started := cloud.tokens[local.Id.String()]
	cloud.mu.Unlock()
	if started {
		t.Fatal("a machine was started for a local job")
	}
}
