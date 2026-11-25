package runners

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/store/db"
)

// RunnerLogin is the credential kind of a job that runs on the runner's own login.
const RunnerLogin = "runner"

// Credentials decides whose model login a job runs on and keeps what the job did to it.
// credentials.Service is one. Without one, every job runs on its runner's own login, as before
// people could bring their own.
type Credentials interface {
	// Choose decides, inside the lease's transaction, which login job runs on: its payer's (open
	// reads the secret into the choice), the runner's own, or none yet — then Wait says why, in
	// words for whoever looks at the job.
	Choose(ctx context.Context, q *db.Queries, job db.Job, open bool) (CredentialChoice, error)
	// Refreshed keeps what the job's agent rewrote of its login.
	Refreshed(ctx context.Context, job db.Job, r proto.CredentialRefresh) error
	// Refused marks the job's login as turned down, and says so to its owner, unless the login
	// proves to work; it says whether it marked it.
	Refused(ctx context.Context, job db.Job, reason string) (bool, error)
	// Sensitive are the values of the job's login, to mask in what the runner reports.
	Sensitive(ctx context.Context, job db.Job) []string
}

// CredentialChoice is the login a job runs on.
type CredentialChoice struct {
	// Kind is RunnerLogin, or the kind of the payer's login.
	Kind string
	ID   pgtype.UUID
	// Exclusive is a login one job at a time may hold.
	Exclusive bool
	// Version is the login's version leased, which a refresh from the job may replace and nothing
	// newer.
	Version int32
	// Credential is the payer's login as the runner hands it to the agent; nil for RunnerLogin, or
	// when not opened.
	Credential *proto.Credential
	// Wait, when set, says why the job cannot be leased yet.
	Wait string
}

// Runner says the job runs on the runner's own login.
func (c CredentialChoice) Runner() bool { return c.Wait == "" && c.Kind == RunnerLogin }

// runnerLogins is the choice of a manager with no Credentials: every job on its runner's login.
type runnerLogins struct{}

func (runnerLogins) Choose(context.Context, *db.Queries, db.Job, bool) (CredentialChoice, error) {
	return CredentialChoice{Kind: RunnerLogin}, nil
}
func (runnerLogins) Refreshed(context.Context, db.Job, proto.CredentialRefresh) error { return nil }
func (runnerLogins) Refused(context.Context, db.Job, string) (bool, error)            { return false, nil }
func (runnerLogins) Sensitive(context.Context, db.Job) []string                       { return nil }
