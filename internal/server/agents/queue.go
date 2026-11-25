package agents

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/store/db"
)

// QueueRunner starts the next turn from a conversation's queue when an agent's turn there ends:
// the first message people queued while it ran asks now, as if just posted. A conversation where
// another turn still runs keeps its queue until that one ends too.
type QueueRunner struct {
	pool *pgxpool.Pool
}

// Handle implements conversations.Handler, on job.finished.
func (r QueueRunner) Handle(ctx context.Context, e db.EventsAfterRow) error {
	q := db.New(r.pool)
	job, err := q.GetJob(ctx, e.AggregateID)
	if err != nil {
		return ignoreGone(err)
	}
	if job.Kind != runners.KindReply || !terminalJob(job.Status) {
		return nil
	}
	turn, err := q.GetTurnByJob(ctx, job.ID)
	if err != nil {
		return ignoreGone(err)
	}
	_, err = conversations.NextQueued(ctx, r.pool, turn.ConversationID)
	return err
}
