package runners

import (
	"context"
	"strings"
	"sync"
	"testing"

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

func (f *fakeLogins) Refused(_ context.Context, _ db.Job, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused = append(f.refused, reason)
	return nil
}

func (f *fakeLogins) Sensitive(context.Context, db.Job) []string { return []string{payerSecret} }

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
	s.caps.PayerLogins = true
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
	s.caps.PayerLogins = true
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
	s.caps.PayerLogins = true
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
	if r := string(done.Receipt); strings.Contains(r, "credential_refresh") || strings.Contains(r, payerSecret) {
		t.Fatalf("receipt %s", r)
	}
	if n := f.count(t, "SELECT count(*) FROM usage_records WHERE job_id = $1 AND paid_by = $2 AND credential_kind = 'claude_subscription'", job.ID, payer); n != 1 {
		t.Fatal("the usage does not say whose login paid")
	}
}
