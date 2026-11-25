package credentials

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// lease is what a runner's lease does, in its order: the lease lock, the queued jobs, then the
// payer's login (marked used).
func (f *fixture) lease(job db.Job) error {
	return store.InTx(f.ctx, f.pool, func(q *db.Queries) error {
		if err := q.LockLeases(f.ctx); err != nil {
			return err
		}
		if _, err := q.LeaseCandidates(f.ctx, db.LeaseCandidatesParams{Backends: []string{job.Backend}, Projects: []pgtype.UUID{job.ProjectID},
			Placement: job.Placement, ReadOnly: []string{job.Backend}, NoTools: []string{job.Backend}, Slots: 50}); err != nil {
			return err
		}
		_, err := f.svc.Choose(f.ctx, q, job, true)
		return err
	})
}

// A person saving or removing a login while runners lease their jobs never deadlocks: both take
// the lease lock first, so the login and the jobs are always locked in one order.
func TestSavingALoginWhileJobsAreLeasedDoesNotDeadlock(t *testing.T) {
	f := newFixture(t)
	ann := f.user("Ann")
	f.put(ann, ClaudeSubscription, claudeToken)
	var jobs []db.Job
	for range 4 {
		jobs = append(jobs, f.job(ann, ann, BackendClaude))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 400)
	for _, j := range jobs {
		wg.Add(1)
		go func(j db.Job) {
			defer wg.Done()
			for range 25 {
				if err := f.lease(j); err != nil {
					errs <- err
				}
			}
		}(j)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 25 {
			// Jobs waiting for a login are what saving one releases: the rows the lease holds too.
			_, _ = f.pool.Exec(context.Background(), "UPDATE jobs SET waiting_reason = 'w' WHERE payer_id = $1 AND status = 'queued'", ann)
			if _, err := f.svc.Put(f.ctx, ann, ClaudeSubscription, claudeToken); err != nil {
				errs <- err
			}
			if i%5 == 4 {
				if err := f.svc.Delete(f.ctx, ann, ClaudeSubscription); err != nil {
					errs <- err
				}
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if strings.Contains(err.Error(), "deadlock") {
			t.Fatalf("deadlock: %v", err)
		}
		t.Errorf("unexpected: %v", err)
	}
}
