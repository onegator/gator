package conversations

import (
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/store/db"
)

// Module is the package's services wired to one database, for the server to hand out. It does
// nothing itself.
type Module struct {
	Access    *Access
	Directory *Directory
	Messages  *Messages
	Threads   *Threads

	pool *pgxpool.Pool
}

// Consumer is the outbox consumer that keeps conversations in step with the rest of Gator:
// people who join a project join its #general. Run it with go consumer.Run(ctx).
func (m *Module) Consumer(hub *events.Hub, log *slog.Logger, poll time.Duration) *Consumer {
	c := NewConsumer(m.pool, hub, log, poll)
	c.On(defaultChannelJoiner{directory: m.Directory}, membershipEvents...)
	return c
}

// New wires the services. roles answers project roles (auth.Authorizer).
func New(pool *pgxpool.Pool, roles RoleResolver) *Module {
	access := NewAccess(roles, db.New(pool))
	return &Module{
		Access:    access,
		Directory: NewDirectory(pool, access, roles),
		Messages:  NewMessages(pool, access),
		Threads:   NewThreads(pool, access),
		pool:      pool,
	}
}
