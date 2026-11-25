package runners

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

const payerSecret = "sk-ant-oat01-the-payers-own-token-0123456789"

// fakeLogins pays every job with a payer on one login, or has it wait when told to.
type fakeLogins struct {
	mu        sync.Mutex
	id        pgtype.UUID
	wait      string
	refreshed []proto.CredentialRefresh
	refused   []string
}

func (f *fakeLogins) Choose(_ context.Context, _ *db.Queries, job db.Job, open bool) (CredentialChoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !job.PayerID.Valid {
		return CredentialChoice{Kind: RunnerLogin}, nil
	}
	if f.wait != "" {
		return CredentialChoice{Wait: f.wait}, nil
	}
	c := CredentialChoice{Kind: "claude_subscription", ID: f.id}
	if open {
		c.Credential = &proto.Credential{ID: uuidString(f.id), Kind: c.Kind, Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": payerSecret},
			Redact: []string{payerSecret}}
	}
	return c, nil
}

func (f *fakeLogins) Refreshed(_ context.Context, _ db.Job, r proto.CredentialRefresh) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, r)
	return nil
}

func (f *fakeLogins) Refused(_ context.Context, _ db.Job, reason string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused = append(f.refused, reason)
	return true, nil
}

func (f *fakeLogins) Sensitive(context.Context, db.Job) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.refreshed) > 0 {
		return []string{payerSecret, rotatedSecret} // the login as the job left it
	}
	return []string{payerSecret}
}

const rotatedSecret = "rt_rotated_while_the_job_ran_0123456789"

// payersJob is a job a person asked for, with fake logins in place: the person and their login are
// real rows, which the lease and the usage refer to.
func (f *fixture) payersJob(t *testing.T) (db.Job, *fakeLogins, pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	var user, cred pgtype.UUID
	if err := f.pool.QueryRow(ctx, "INSERT INTO users(email,name,workspace_role) VALUES($1,'Payer','member') RETURNING id",
		"payer-"+suffix()+"@t.local").Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, "INSERT INTO user_credentials(user_id, kind, backend, secret) VALUES($1,'claude_subscription',$2,'sealed') RETURNING id",
		user, f.backend).Scan(&cred); err != nil {
		t.Fatal(err)
	}
	logins := &fakeLogins{id: cred}
	f.m.cfg.Credentials = logins
	task, err := f.m.process.Create(ctx, process.CreateParams{ProjectID: f.project, Kind: "bug", Title: "Crash " + suffix()}, process.Actor{Kind: process.ActorUser, ID: user})
	if err != nil {
		t.Fatal(err)
	}
	f.allowAgents(t, task.ID)
	job, err := f.m.CreateJob(ctx, task.ID, NewJob{Role: "implementer", Backend: f.backend, CreatedBy: process.Actor{Kind: process.ActorUser, ID: user}})
	if err != nil {
		t.Fatal(err)
	}
	if job.PayerID != user {
		t.Fatalf("payer %v, want %v", job.PayerID, user)
	}
	return job, logins, user
}

// A job on its payer's login goes only to a runner that says it runs payers' logins — whatever
// that runner's own login is — and is handed over with the login.
func TestLeaseHandsThePayersLoginOnlyToRunnersThatRunIt(t *testing.T) {
	f := newFixture(t)
	job, _, _ := f.payersJob(t)
	old := f.session(t)
	if got, err := old.lease(context.Background(), 2); err != nil || len(got) != 0 {
		t.Fatalf("a runner that would use its own login was leased %v (%v)", got, err)
	}
	s := f.session(t)
	s.caps.PayerLogins = []string{f.backend}
	s.auth = map[string]string{f.backend: "missing"} // its own login does not matter
	got, err := s.lease(context.Background(), 2)
	if err != nil || len(got) != 1 || got[0].job.ID != job.ID {
		t.Fatalf("lease %v %v", got, err)
	}
	if got[0].job.CredentialKind != "claude_subscription" || got[0].credential == nil {
		t.Fatalf("leased %+v", got[0])
	}
	pj := s.handOver(context.Background(), f.q, got[0].job, got[0].credential)
	if pj.Credential == nil || pj.Credential.Env["CLAUDE_CODE_OAUTH_TOKEN"] != payerSecret {
		t.Fatalf("handed %+v", pj.Credential)
	}
}

// A job whose payer cannot pay is not leased, and says why — once, not on every look.
func TestAJobThatCannotBePaidForWaitsAndSaysWhy(t *testing.T) {
	f := newFixture(t)
	job, logins, _ := f.payersJob(t)
	logins.wait = "Waiting for Payer to connect a Claude subscription"
	s := f.session(t)
	s.caps.PayerLogins = []string{f.backend}
	for range 2 {
		if got, err := s.lease(context.Background(), 2); err != nil || len(got) != 0 {
			t.Fatalf("lease %v %v", got, err)
		}
	}
	if got := f.job(t, job.ID); got.WaitingReason != logins.wait || got.Status != "queued" {
		t.Fatalf("job %q %q", got.Status, got.WaitingReason)
	}
	if n := f.count(t, "SELECT count(*) FROM events WHERE type = 'job.waiting' AND aggregate_id = $1", job.ID); n != 1 {
		t.Fatalf("said %d times", n)
	}
}

// What a job reports is masked of its login's values; what its agent rewrote of the login is kept
// with the login, never in the receipt; a refused login fails the job and is reported; and the
// usage says whose login paid.
func TestReportsOfAJobOnItsPayersLogin(t *testing.T) {
	f := newFixture(t)
	job, logins, payer := f.payersJob(t)
	s := f.session(t)
	s.caps.PayerLogins = []string{f.backend}
	got, err := s.lease(context.Background(), 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("lease %v %v", got, err)
	}
	id := uuidString(job.ID)
	if err := s.events(proto.Events{JobID: id, Events: []proto.JobEvent{{Seq: 1, Type: "text",
		Payload: []byte(`{"text":"token ` + payerSecret + `"}`)}}}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM job_events WHERE job_id = $1 AND payload::text LIKE '%'||$2||'%'", job.ID, payerSecret); n != 0 {
		t.Fatal("an event kept the login")
	}
	if n := f.count(t, "SELECT count(*) FROM job_events WHERE job_id = $1 AND payload::text LIKE '%[redacted]%'", job.ID); n != 1 {
		t.Fatal("the event was not masked")
	}
	fin := proto.Finish{JobID: id, Status: proto.StatusFailed, StopReason: "claude: Invalid API key (" + payerSecret + ")", Usage: usage,
		Summary:           "rotated to " + rotatedSecret,
		CredentialRefresh: &proto.CredentialRefresh{ID: uuidString(logins.id), Files: map[string][]byte{"auth.json": []byte(`{"new":true}`)}},
		CredentialRefused: "Invalid API key"}
	if err := s.finish(fin); err != nil {
		t.Fatal(err)
	}
	if len(logins.refreshed) != 1 || len(logins.refused) != 1 {
		t.Fatalf("refreshed %v refused %v", logins.refreshed, logins.refused)
	}
	done := f.job(t, job.ID)
	if done.Status != proto.StatusFailed || !strings.Contains(*done.StopReason, "turned down") {
		t.Fatalf("job %s %v", done.Status, *done.StopReason)
	}
	if r := string(done.Receipt); strings.Contains(r, "credential_refresh") || strings.Contains(r, payerSecret) || strings.Contains(r, rotatedSecret) {
		t.Fatalf("receipt %s", r)
	}
	if n := f.count(t, "SELECT count(*) FROM usage_records WHERE job_id = $1 AND paid_by = $2 AND credential_kind = 'claude_subscription'", job.ID, payer); n != 1 {
		t.Fatal("the usage does not say whose login paid")
	}
}

// A cloud job waiting for its payer's login gets no machine started for it.
func TestNoMachineForAJobThatWaitsForALogin(t *testing.T) {
	f := newFixture(t)
	job, _, _ := f.payersJob(t)
	if _, err := f.pool.Exec(context.Background(), "UPDATE jobs SET placement = 'cloud', waiting_reason = 'w' WHERE id = $1", job.ID); err != nil {
		t.Fatal(err)
	}
	queued, err := f.q.ListQueuedCloudJobs(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range queued {
		if j.ID == job.ID {
			t.Fatal("a machine would start for a job nobody can pay for")
		}
	}
}

// A job nobody asked for in person — the autopilot's, a summary's — but paid for by Bob is never
// offered to Ann's Mac with Bob's login; Bob's own machine and a shared runner take it.
func TestAPayersLoginGoesOnlyToTheirOwnOrASharedRunner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, logins, bob := f.payersJob(t) // Bob's own job, with his login in place
	if _, err := f.pool.Exec(ctx, "UPDATE jobs SET status = 'stopped' WHERE payer_id = $1", bob); err != nil {
		t.Fatal(err)
	}
	task, err := f.m.process.Create(ctx, process.CreateParams{ProjectID: f.project, Kind: "bug", Title: "Bob's " + suffix()}, process.Actor{Kind: process.ActorUser, ID: bob})
	if err != nil {
		t.Fatal(err)
	}
	f.allowAgents(t, task.ID)
	job, err := f.m.CreateJob(ctx, task.ID, NewJob{Role: "implementer", Backend: f.backend, CreatedBy: process.Actor{Kind: process.ActorSystem}})
	if err != nil || job.PayerID != bob {
		t.Fatalf("job %v payer %v", err, job.PayerID)
	}
	f.m.cfg.Credentials = logins
	runner := func(owner pgtype.UUID) *session {
		s := f.session(t)
		s.caps.PayerLogins, s.owner = []string{f.backend}, owner
		return s
	}
	if got, err := runner(randomUUID()).lease(ctx, 2); err != nil || len(got) != 0 {
		t.Fatalf("Ann's Mac was offered Bob's login: %v %v", got, err)
	}
	// Past the database's filter too: the session itself refuses.
	if got, err := f.q.LeaseCandidates(ctx, db.LeaseCandidatesParams{Backends: []string{f.backend}, Projects: []pgtype.UUID{f.project},
		Placement: proto.PlacementLocal, Owner: randomUUID(), ReadOnly: []string{f.backend}, NoTools: []string{f.backend}, Slots: 10}); err != nil || len(got) != 0 {
		t.Fatalf("candidates for Ann's Mac: %v %v", len(got), err)
	}
	if got, err := runner(bob).lease(ctx, 2); err != nil || len(got) != 1 {
		t.Fatalf("Bob's own Mac: %v %v", got, err)
	}
}

// The session refuses a payer's login for another person's machine even when the database offers it.
func TestTheSessionRefusesAnotherPersonsLogin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	job, _, _ := f.payersJob(t)
	// The login is not for this backend in the database, so the filter lets the job through;
	// the fake still chooses it.
	if _, err := f.pool.Exec(ctx, "UPDATE user_credentials SET backend = 'elsewhere' WHERE user_id = $1", job.PayerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE jobs SET created_by_kind = 'system', created_by = NULL WHERE id = $1", job.ID); err != nil {
		t.Fatal(err)
	}
	s := f.session(t)
	s.caps.PayerLogins, s.owner = []string{f.backend}, randomUUID()
	if got, err := s.lease(ctx, 2); err != nil || len(got) != 0 {
		t.Fatalf("leased %v %v", got, err)
	}
	shared := f.session(t)
	shared.caps.PayerLogins = []string{f.backend}
	if got, err := shared.lease(ctx, 2); err != nil || len(got) != 1 {
		t.Fatalf("a shared runner: %v %v", got, err)
	}
}

// Jobs on one runner run as one user and can read each other's environment: a person's login runs
// only beside jobs on that same person's login, never beside another person's or the runner's own.
func TestOneRunnerHoldsOnePayersLoginAtATime(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bobs, logins, _ := f.payersJob(t)
	carols, _, _ := f.payersJob(t) // another payer; same fake login row, another person
	f.m.cfg.Credentials = logins
	s := f.session(t)
	s.caps.PayerLogins, s.caps.MaxParallel = []string{f.backend}, 4
	got, err := s.lease(ctx, 4)
	if err != nil || len(got) != 1 {
		t.Fatalf("leased %d (%v), want one payer's job", len(got), err)
	}
	other := carols
	if got[0].job.ID == carols.ID {
		other = bobs
	}
	if again, _ := s.lease(ctx, 4); len(again) != 0 {
		t.Fatalf("%v ran beside another payer's job", again)
	}
	// A job on the runner's own login waits too while a payer's is there.
	runners := f.queuedJob(t)
	if again, _ := s.lease(ctx, 4); len(again) != 0 {
		t.Fatalf("the runner's own job ran beside a payer's: %v", again)
	}
	if err := s.finish(proto.Finish{JobID: uuidString(got[0].job.ID), Status: proto.StatusFailed}); err != nil {
		t.Fatal(err)
	}
	next, _ := s.lease(ctx, 4)
	ids := map[pgtype.UUID]bool{}
	for _, l := range next {
		ids[l.job.ID] = true
	}
	if len(next) != 1 || !(ids[other.ID] || ids[runners.ID]) {
		t.Fatalf("after the first ended: %v", next)
	}
}

// The plugins judging whether a turn may end read what the agent said, masked of its login.
func TestTheJudgeReadsNothingOfTheLogin(t *testing.T) {
	f := newFixture(t)
	job, _, _ := f.payersJob(t)
	s := f.session(t)
	s.caps.PayerLogins = []string{f.backend}
	if _, err := s.lease(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	job = f.job(t, job.ID)
	te := s.maskTurnEnding(context.Background(), job, proto.TurnEnding{JobID: uuidString(job.ID), Summary: "used " + payerSecret,
		Digest: &proto.Digest{Left: []string{"rotate " + payerSecret}}})
	if strings.Contains(te.Summary+strings.Join(te.Digest.Left, ""), payerSecret) {
		t.Fatalf("the judge would read %+v", te)
	}
}

// A job that waited because another job held its payer's ChatGPT login stops waiting once that
// job ends — or, when it went another way (stopped, its lease lost), on the next sweep — so that a
// cloud job gets its machine.
func TestAJobWaitingForABusyLoginIsReleased(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, logins, payer := f.payersJob(t)
	s := f.session(t)
	s.caps.PayerLogins = []string{f.backend}
	if got, err := s.lease(ctx, 1); err != nil || len(got) != 1 {
		t.Fatalf("lease %v %v", got, err)
	}
	f.m.cfg.Credentials = logins
	task, _ := f.m.process.Create(ctx, process.CreateParams{ProjectID: f.project, Kind: "bug", Title: "next " + suffix()}, process.Actor{Kind: process.ActorUser, ID: payer})
	f.allowAgents(t, task.ID)
	second, err := f.m.CreateJob(ctx, task.ID, NewJob{Role: "implementer", Backend: f.backend, CreatedBy: process.Actor{Kind: process.ActorUser, ID: payer}})
	if err != nil {
		t.Fatal(err)
	}
	busy := func() {
		if _, err := f.pool.Exec(ctx, "UPDATE jobs SET credential_exclusive = (id = $1), waiting_reason = CASE WHEN id = $2 THEN 'busy' ELSE '' END WHERE id IN ($1, $2)",
			first.ID, second.ID); err != nil {
			t.Fatal(err)
		}
	}
	busy()
	if _, err := f.m.Sweep(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if f.job(t, second.ID).WaitingReason != "busy" {
		t.Fatal("released while the login was still held")
	}
	if err := s.finish(proto.Finish{JobID: uuidString(first.ID), Status: proto.StatusFailed}); err != nil {
		t.Fatal(err)
	}
	if r := f.job(t, second.ID).WaitingReason; r != "" {
		t.Fatalf("still waiting after the holder ended: %q", r)
	}
	// Ended another way: the sweep notices.
	if _, err := f.pool.Exec(ctx, "UPDATE jobs SET waiting_reason = 'busy' WHERE id = $1", second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Sweep(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r := f.job(t, second.ID).WaitingReason; r != "" {
		t.Fatalf("the sweep left it waiting: %q", r)
	}
}
