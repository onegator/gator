package conversations

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store/db"
)

// taskThreadOpener gives every new task its thread, whoever created the task: a person through
// the API, a plugin, an automation.
type taskThreadOpener struct {
	pool    *pgxpool.Pool
	threads *Threads
}

// Handle implements Handler.
func (o taskThreadOpener) Handle(ctx context.Context, e db.EventsAfterRow) error {
	task, err := db.New(o.pool).GetTask(ctx, e.AggregateID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // deleted since
	}
	if err != nil {
		return err
	}
	_, err = o.threads.ForTask(ctx, task)
	return err
}
