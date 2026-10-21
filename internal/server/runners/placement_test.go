package runners

import (
	"context"
	"crypto/rand"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

func randomUUID() pgtype.UUID {
	var u pgtype.UUID
	_, _ = rand.Read(u.Bytes[:])
	u.Valid = true
	return u
}

// jobFor queues a job in the fixture's project with the given placement, started by actor.
func (f *fixture) jobFor(t *testing.T, placement string, actor process.Actor) db.Job {
	t.Helper()
	ctx := context.Background()
	task, err := f.m.process.Create(ctx, process.CreateParams{ProjectID: f.project, Kind: "bug", Title: "Crash " + suffix()}, process.Actor{Kind: process.ActorSystem})
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.m.CreateJob(ctx, task.ID, NewJob{Role: "implementer", Backend: f.backend, Placement: placement, CreatedBy: actor})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// lease asks the database what s would be handed now, as dispatch does.
func (f *fixture) lease(t *testing.T, s *session) []db.Job {
	t.Helper()
	jobs, err := f.q.LeaseJobs(context.Background(), db.LeaseJobsParams{RunnerID: s.runnerID, LeaseUntil: ts(time.Now().Add(time.Minute)),
		Backends: []string{f.backend}, Projects: []pgtype.UUID{f.project}, Slots: 10,
		Placement: s.placementOrLocal(), OnlyJob: s.onlyJob, Owner: s.owner})
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func ids(jobs []db.Job) map[pgtype.UUID]bool {
	out := map[pgtype.UUID]bool{}
	for _, j := range jobs {
		out[j.ID] = true
	}
	return out
}

// A job meant for someone's Mac never lands on a cloud runner, and a cloud job never on a Mac.
func TestLeaseMatchesPlacement(t *testing.T) {
	f := newFixture(t)
	system := process.Actor{Kind: process.ActorSystem}
	local := f.jobFor(t, proto.PlacementLocal, system)
	cloud := f.jobFor(t, proto.PlacementCloud, system)
	if local.Placement != proto.PlacementLocal || cloud.Placement != proto.PlacementCloud {
		t.Fatalf("placements %q %q", local.Placement, cloud.Placement)
	}

	cloudRunner := f.session(t)
	cloudRunner.placement = proto.PlacementCloud
	got := ids(f.lease(t, cloudRunner))
	if got[local.ID] || !got[cloud.ID] || len(got) != 1 {
		t.Fatalf("the cloud runner leased %v", got)
	}
	got = ids(f.lease(t, f.s))
	if got[cloud.ID] || !got[local.ID] || len(got) != 1 {
		t.Fatalf("the local runner leased %v", got)
	}
}

// A local job someone started runs on their machine or a shared one — never on another
// person's Mac, which is logged in as that person. The autopilot's jobs have nobody behind
// them and run on any local runner.
func TestLocalJobGoesToTheMachineOfWhoeverStartedIt(t *testing.T) {
	f := newFixture(t)
	alice, bob := randomUUID(), randomUUID()
	byAlice := f.jobFor(t, proto.PlacementLocal, process.Actor{Kind: process.ActorUser, ID: alice})

	bobs := f.session(t)
	bobs.owner = bob
	if got := f.lease(t, bobs); len(got) != 0 {
		t.Fatalf("Bob's Mac took Alice's job: %v", ids(got))
	}
	alices := f.session(t)
	alices.owner = alice
	if got := ids(f.lease(t, alices)); !got[byAlice.ID] {
		t.Fatalf("Alice's Mac did not get her job: %v", got)
	}

	shared := f.jobFor(t, proto.PlacementLocal, process.Actor{Kind: process.ActorUser, ID: alice})
	if got := ids(f.lease(t, f.s)); !got[shared.ID] { // f.s has no owner: a shared runner
		t.Fatalf("a shared runner did not take a person's local job: %v", got)
	}

	auto := f.jobFor(t, proto.PlacementLocal, process.Actor{Kind: process.ActorSystem})
	if got := ids(f.lease(t, bobs)); !got[auto.ID] {
		t.Fatalf("the autopilot's job did not go to an available Mac: %v", got)
	}
}

// A runner started for one job takes that job and nothing else.
func TestOneJobRunnerTakesOnlyItsJob(t *testing.T) {
	f := newFixture(t)
	system := process.Actor{Kind: process.ActorSystem}
	first := f.jobFor(t, proto.PlacementCloud, system)
	mine := f.jobFor(t, proto.PlacementCloud, system)
	s := f.session(t)
	s.placement, s.onlyJob = proto.PlacementCloud, mine.ID
	got := ids(f.lease(t, s))
	if len(got) != 1 || !got[mine.ID] || got[first.ID] {
		t.Fatalf("a one-job runner leased %v", got)
	}
}

func TestAProjectChoosesWhereItsAutopilotRuns(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE projects SET process_config = '{"policy":{"placement":"cloud"}}' WHERE id = $1`, f.project); err != nil {
		t.Fatal(err)
	}
	if j := f.jobFor(t, "", process.Actor{Kind: process.ActorSystem}); j.Placement != proto.PlacementCloud {
		t.Fatalf("placement %q, want the project's cloud", j.Placement)
	}
	if j := f.jobFor(t, proto.PlacementLocal, process.Actor{Kind: process.ActorSystem}); j.Placement != proto.PlacementLocal {
		t.Fatalf("an explicit placement was overridden: %q", j.Placement)
	}
	task, _ := f.m.process.Create(context.Background(), process.CreateParams{ProjectID: f.project, Kind: "bug", Title: "x"}, process.Actor{Kind: process.ActorSystem})
	if _, err := f.m.CreateJob(context.Background(), task.ID, NewJob{Role: "implementer", Backend: f.backend, Placement: "moon"}); err == nil {
		t.Fatal("an unknown placement was accepted")
	}
}

func TestServeRecordsWhereARunnerTakesWorkFrom(t *testing.T) {
	f := newFixture(t)
	register := func(p auth.Principal, reg proto.Register) (proto.MessageType, db.Runner) {
		t.Helper()
		w, _ := f.dialServe(t, p)
		w.send(proto.TypeRegister, 1, reg)
		env, ok := w.next(5 * time.Second)
		if !ok {
			t.Fatal("no answer")
		}
		var r proto.Registered
		if env.Type != proto.TypeRegistered || env.Into(&r) != nil {
			return env.Type, db.Runner{}
		}
		id, _ := parseUUID(r.RunnerID)
		row, err := f.q.GetRunner(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return env.Type, row
	}
	caps := proto.Capabilities{Backends: []string{f.backend}}

	if _, r := register(f.principal(t), proto.Register{Location: "vps", Capabilities: caps}); r.Placement != proto.PlacementCloud {
		t.Fatalf("a VPS that names no placement is %q, want cloud", r.Placement)
	}
	if _, r := register(f.principal(t), proto.Register{Location: "mac", Capabilities: caps}); r.Placement != proto.PlacementLocal {
		t.Fatalf("a Mac that names no placement is %q, want local", r.Placement)
	}
	if _, r := register(f.principal(t), proto.Register{Location: "vps", Placement: "local", Capabilities: caps}); r.Placement != proto.PlacementLocal {
		t.Fatalf("an explicit placement was ignored: %q", r.Placement)
	}
	if typ, _ := register(f.principal(t), proto.Register{Placement: "moon", Capabilities: caps}); typ != proto.TypeError {
		t.Fatalf("an unknown placement registered: %s", typ)
	}
	bad := proto.Capabilities{Backends: caps.Backends, Projects: []string{"my-project"}}
	if typ, _ := register(f.principal(t), proto.Register{Capabilities: bad}); typ != proto.TypeError {
		t.Fatalf("a project slug registered, and would have meant any project: %s", typ)
	}

	// A token minted for one job is a cloud runner for that job's project, whatever it says.
	job := f.jobFor(t, proto.PlacementCloud, process.Actor{Kind: process.ActorSystem})
	p := f.principal(t)
	p.JobID, p.ProjectID = job.ID, f.project
	_, r := register(p, proto.Register{Location: "mac", Placement: "local", Capabilities: caps})
	if r.Placement != proto.PlacementCloud {
		t.Fatalf("a one-job runner registered as %q", r.Placement)
	}
}

type fakeProvisioner struct {
	mu      sync.Mutex
	started map[string][]MachineSpec // job id → machines started for it
	stopped []string
	fail    error
}

func (p *fakeProvisioner) Name() string { return "fake" }

func (p *fakeProvisioner) Start(_ context.Context, spec MachineSpec) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return "", p.fail
	}
	p.started[spec.JobID] = append(p.started[spec.JobID], spec)
	return "machine-" + spec.Name, nil
}

func (p *fakeProvisioner) Stop(_ context.Context, handle string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = append(p.stopped, handle)
	return nil
}

func (p *fakeProvisioner) starts(jobID pgtype.UUID) []MachineSpec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started[uuidString(jobID)]
}

type fakeRunnerTokens struct{ issued []pgtype.UUID }

func (f *fakeRunnerTokens) IssueRunnerForJob(_ context.Context, jobID, _ pgtype.UUID, _ string, _ time.Duration) (string, error) {
	f.issued = append(f.issued, jobID)
	return "gtr_runner_one_job", nil
}

func (f *fixture) withProvisioner(t *testing.T) (*fakeProvisioner, *fakeRunnerTokens) {
	t.Helper()
	p := &fakeProvisioner{started: map[string][]MachineSpec{}}
	tokens := &fakeRunnerTokens{}
	f.m.cfg.Provisioner, f.m.cfg.RunnerTokens = p, tokens
	f.m.cfg.CloudServerURL = "https://gator.example"
	f.m.cfg.CloudEnv = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth", "GATOR_RUNNER_TOKEN": "must be replaced"}
	f.m.cfg.ProvisionDelay = time.Hour // tests call Provision themselves
	return p, tokens
}

// A cloud job nobody can take gets a machine of its own, with a token for that job, and the
// machine goes when the job ends.
func TestProvisionStartsAMachineForACloudJobAndStopsItAfter(t *testing.T) {
	f := newFixture(t)
	prov, tokens := f.withProvisioner(t)
	ctx := context.Background()
	local := f.jobFor(t, proto.PlacementLocal, process.Actor{Kind: process.ActorSystem})
	job := f.jobFor(t, proto.PlacementCloud, process.Actor{Kind: process.ActorSystem})

	f.m.Provision(ctx)
	if len(prov.starts(local.ID)) != 0 {
		t.Fatal("a machine was started for a local job")
	}
	starts := prov.starts(job.ID)
	if len(starts) != 1 || len(tokens.issued) == 0 {
		t.Fatalf("starts %v, tokens %v", starts, tokens.issued)
	}
	env := starts[0].Env
	for k, want := range map[string]string{
		"GATOR_RUNNER_TOKEN": "gtr_runner_one_job", "GATOR_RUNNER_SERVER_URL": "https://gator.example",
		"GATOR_RUNNER_PLACEMENT": "cloud", "GATOR_RUNNER_ONCE": "1", "GATOR_RUNNER_BACKENDS": f.backend,
		"GATOR_RUNNER_PROJECTS": uuidString(f.project), "CLAUDE_CODE_OAUTH_TOKEN": "oauth",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}

	f.m.Provision(ctx)
	if n := len(prov.starts(job.ID)); n != 1 {
		t.Fatalf("a second machine was started while the first is on its way (%d)", n)
	}

	if err := f.m.StopJob(ctx, job.ID, "enough"); err != nil {
		t.Fatal(err)
	}
	f.m.Provision(ctx)
	prov.mu.Lock()
	stopped := append([]string(nil), prov.stopped...)
	prov.mu.Unlock()
	want := "machine-" + starts[0].Name
	if !slices.Contains(stopped, want) {
		t.Fatalf("the machine outlived its job: stopped %v", stopped)
	}
	if ev := f.jobEvents(t, job.ID); !slices.Contains(ev, "job.provisioned") {
		t.Fatalf("events %v", ev)
	}
}

// A connected cloud runner with room takes the job; nothing needs starting.
func TestProvisionLeavesTheJobToAnIdleCloudRunner(t *testing.T) {
	f := newFixture(t)
	prov, _ := f.withProvisioner(t)
	// Queued before the runner is in the map: this session has no connection to offer it on.
	job := f.jobFor(t, proto.PlacementCloud, process.Actor{Kind: process.ActorSystem})
	s := f.session(t)
	s.placement = proto.PlacementCloud
	f.m.mu.Lock()
	f.m.sessions[s.runnerID.Bytes] = s
	f.m.mu.Unlock()
	t.Cleanup(func() { f.m.dropSession(s) })

	f.m.Provision(context.Background())
	if n := len(prov.starts(job.ID)); n != 0 {
		t.Fatalf("started %d machines next to an idle cloud runner", n)
	}
}

// A machine that never takes its job is replaced, a few times, and then the job stops with the
// reason instead of starting machines forever.
func TestProvisionGivesUpOnAJobNoMachineTakes(t *testing.T) {
	f := newFixture(t)
	prov, _ := f.withProvisioner(t)
	f.m.cfg.ProvisionTimeout = time.Nanosecond
	f.m.cfg.MaxProvisions = 2
	job := f.jobFor(t, proto.PlacementCloud, process.Actor{Kind: process.ActorSystem})
	for range 4 {
		f.m.Provision(context.Background())
	}
	if n := len(prov.starts(job.ID)); n != 2 {
		t.Fatalf("started %d machines, want 2", n)
	}
	if j := f.job(t, job.ID); j.Status != proto.StatusStopped || j.StopReason == nil {
		t.Fatalf("job is %s (%v)", j.Status, j.StopReason)
	}
}

// With a provisioner a queued cloud job is not "nobody can take it": its runner is coming.
func TestUnassignableSkipsCloudJobsAProvisionerHandles(t *testing.T) {
	f := newFixture(t)
	f.withProvisioner(t)
	f.m.cfg.UnassignableAfter = time.Nanosecond
	job := f.jobFor(t, proto.PlacementCloud, process.Actor{Kind: process.ActorSystem})
	if _, err := f.m.Sweep(context.Background(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := f.job(t, job.ID); j.UnassignableSince.Valid {
		t.Fatal("a cloud job was called unassignable while its machine is on the way")
	}
	f.m.cfg.Provisioner = nil
	if _, err := f.m.Sweep(context.Background(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if j := f.job(t, job.ID); !j.UnassignableSince.Valid {
		t.Fatal("without a provisioner nobody takes a cloud job, and the sweep should say so")
	}
}
