package runners

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// leaseJobs leases to runner every job it could be handed, on its own login: what the database
// offers, without the session deciding whose login each runs on.
func leaseJobs(ctx context.Context, q *db.Queries, runner pgtype.UUID, until pgtype.Timestamptz, p db.LeaseCandidatesParams) ([]db.Job, error) {
	jobs, err := q.LeaseCandidates(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]db.Job, 0, len(jobs))
	for _, j := range jobs {
		leased, err := q.LeaseJob(ctx, db.LeaseJobParams{ID: j.ID, RunnerID: runner, LeaseUntil: until, CredentialKind: RunnerLogin})
		if err != nil {
			return nil, err
		}
		out = append(out, leased)
	}
	return out, nil
}
