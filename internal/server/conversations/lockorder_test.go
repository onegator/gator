package conversations

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Opening a task's thread while its project is being deleted never deadlocks: the thread's
// transaction takes the project and the task before the channel, the order the deletion cascades
// in. (It used to hold the channel and its card, then wait for the project the deletion held.)
func TestOpeningATaskThreadDoesNotDeadlockWithItsProjectsDeletion(t *testing.T) {
	f := newFixture(t)
	threads := NewThreads(f.pool, f.access, f.directory)
	for i := range 30 {
		project := f.project(nil)
		if _, err := f.directory.EnsureDefaultChannels(f.ctx, project); err != nil {
			t.Fatal(err)
		}
		var task pgtype.UUID
		if err := f.pool.QueryRow(f.ctx, `INSERT INTO tasks (project_id, kind, title, phase) VALUES ($1, 'bug', $2, 'planning') RETURNING id`,
			project, fmt.Sprintf("t%d", i)).Scan(&task); err != nil {
			t.Fatal(err)
		}
		row, err := dbTask(f, task)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var openErr, deleteErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, openErr = threads.ForTask(f.ctx, row) }()
		go func() {
			defer wg.Done()
			_, deleteErr = f.pool.Exec(f.ctx, `DELETE FROM projects WHERE id = $1`, project)
		}()
		wg.Wait()
		for _, err := range []error{openErr, deleteErr} {
			if pe, ok := errors.AsType[*pgconn.PgError](err); ok && pe.Code == "40P01" {
				t.Fatalf("round %d: deadlock: %v", i, err)
			}
		}
	}
}

func dbTask(f *fixture, id pgtype.UUID) (db.Task, error) { return db.New(f.pool).GetTask(f.ctx, id) }
