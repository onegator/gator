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
}

// NewConsumer builds a consumer with no handlers; poll defaults to two seconds.
func NewConsumer(pool *pgxpool.Pool, hub *events.Hub, log *slog.Logger, poll time.Duration) *Consumer {
	if poll == 0 {
		poll = 2 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Consumer{pool: pool, hub: hub, log: log, poll: poll, handlers: map[string][]Handler{}}
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

// Drain handles every event after the cursor once (at least once, across crashes).
func (c *Consumer) Drain(ctx context.Context) {
	if len(c.types) == 0 {
		return
	}
	events.DrainCursor(ctx, c.pool, cursorName, c.types, func(r db.EventsAfterRow) {
		for _, h := range c.handlers[r.Type] {
			if err := h.Handle(ctx, r); err != nil && ctx.Err() == nil {
				c.log.Warn("conversations", "event", r.Type, "id", r.ID, "err", err)
			}
		}
	})
}
