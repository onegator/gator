package conversations

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/store/db"
)

// cursorName is this package's row in event_cursors.
const cursorName = "conversations"

// Handler reacts to one event from the outbox. Delivery is at least once, so a handler must
// tolerate seeing an event again: everything here is idempotent (a card is updated, a member
// joins once).
type Handler interface {
	Handle(ctx context.Context, e db.EventsAfterRow) error
}

// HandlerFunc is a function that is a Handler.
type HandlerFunc func(ctx context.Context, e db.EventsAfterRow) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, e db.EventsAfterRow) error { return f(ctx, e) }

// Consumer reads the outbox with its own cursor, like the product curator, and hands each event
// to the handlers that watch its type. What to do with an event is the handlers' business; a new
// kind of reaction is one more On, not a change here.
type Consumer struct {
	pool     *pgxpool.Pool
	hub      *events.Hub
	log      *slog.Logger
	poll     time.Duration
	handlers map[string][]Handler
	types    []string
	// attempts counts the failed tries of the event the drain is stuck on.
	attempts map[int64]int
}

// NewConsumer builds a consumer with no handlers; poll defaults to two seconds.
func NewConsumer(pool *pgxpool.Pool, hub *events.Hub, log *slog.Logger, poll time.Duration) *Consumer {
	if poll == 0 {
		poll = 2 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Consumer{pool: pool, hub: hub, log: log, poll: poll, handlers: map[string][]Handler{}, attempts: map[int64]int{}}
}

// On has h handle every event of these types, after the handlers already registered for them.
func (c *Consumer) On(h Handler, types ...string) {
	for _, t := range types {
		if _, seen := c.handlers[t]; !seen {
			c.types = append(c.types, t)
		}
		c.handlers[t] = append(c.handlers[t], h)
	}
}

// Run handles events until ctx ends. It wakes on every hub event and on a timer.
func (c *Consumer) Run(ctx context.Context) {
	wake, unsub := c.hub.Subscribe("*")
	defer unsub()
	if err := db.New(c.pool).InitEventCursor(ctx, cursorName); err != nil && ctx.Err() == nil {
		c.log.Warn("conversations cursor", "err", err)
	}
	t := time.NewTicker(c.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			c.Drain(ctx)
		case <-t.C:
			c.Drain(ctx)
		}
	}
}

// maxAttempts is how often one event is tried before it is given up on, so that one event no
// handler can take does not stop every conversation from following the work.
const maxAttempts = 5

// Drain handles the events after the cursor in order, at least once. A handler that fails stops
// the drain at that event, and the next drain tries it again; after maxAttempts it is logged
// and passed over.
func (c *Consumer) Drain(ctx context.Context) {
	if len(c.types) == 0 {
		return
	}
	err := events.DrainCursorUntil(ctx, c.pool, cursorName, c.types, func(r db.EventsAfterRow) error {
		for _, h := range c.handlers[r.Type] {
			if err := h.Handle(ctx, r); err != nil {
				if c.attempts[r.ID]++; c.attempts[r.ID] < maxAttempts {
					return err
				}
				c.log.Error("conversations: giving up on an event", "event", r.Type, "id", r.ID, "err", err)
			}
		}
		delete(c.attempts, r.ID)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		c.log.Warn("conversations", "err", err)
	}
}
