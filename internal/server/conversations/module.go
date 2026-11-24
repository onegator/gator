package conversations

import (
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/store/db"
)

// Deps are what the package needs from the rest of the server.
type Deps struct {
	// Roles answers a caller's role in a project (auth.Authorizer).
	Roles RoleResolver
	// Decisions answers whether a task waits on a person (process.Service). Nil: no decision
	// cards.
	Decisions DecisionSource
	// Tasks creates tasks (process.Service). Nil: messages cannot be made into tasks.
	Tasks TaskCreator
	// Blobs keeps uploaded files (BlobStoreFromEnv). Nil: no attachments.
	Blobs BlobStore
}

// Module is the package's services wired to one database, for the server to hand out. It does
// nothing itself.
type Module struct {
	Access    *Access
	Directory *Directory
	Messages  *Messages
	Threads   *Threads
	Cards     *Projector
	Search    *Search
	// Promoter is nil without Deps.Tasks.
	Promoter *Promoter
	// Attachments is nil without Deps.Blobs.
	Attachments *Attachments

	pool *pgxpool.Pool
	deps Deps
}

// New wires the services.
func New(pool *pgxpool.Pool, deps Deps) *Module {
	access := NewAccess(deps.Roles, db.New(pool))
	directory := NewDirectory(pool, access, deps.Roles)
	threads := NewThreads(pool, access, directory)
	m := &Module{
		Access:    access,
		Directory: directory,
		Messages:  NewMessages(pool, access),
		Threads:   threads,
		Cards:     NewProjector(pool, threads),
		Search:    NewSearch(pool, access),
		pool:      pool,
		deps:      deps,
	}
	if deps.Tasks != nil {
		m.Promoter = NewPromoter(pool, access, deps.Roles, deps.Tasks)
	}
	if deps.Blobs != nil {
		m.Attachments = NewAttachments(pool, access, deps.Blobs)
		m.Messages.attach = m.Attachments
	}
	return m
}

// CardSources are the kinds of cards work leaves in conversations: each task's own card (which
// opens its thread), the decision its gate waits for, its jobs and what its phases produced.
func (m *Module) CardSources() []CardSource {
	q := db.New(m.pool)
	sources := []CardSource{taskCards{tasks: q}, jobCards{jobs: q}, artifactCards{artifacts: q}}
	if m.deps.Decisions != nil {
		sources = append(sources, decisionCards{tasks: q, decisions: m.deps.Decisions, index: m.Cards})
	}
	return sources
}

// Consumer is the outbox consumer that keeps conversations in step with the rest of Gator:
// people who join a project join its #general, and work shows up as cards (every new task gets
// its thread with the first). Run it with go consumer.Run(ctx).
func (m *Module) Consumer(hub *events.Hub, log *slog.Logger, poll time.Duration) *Consumer {
	c := NewConsumer(m.pool, hub, log, poll)
	c.On(defaultChannelJoiner{directory: m.Directory}, membershipEvents...)
	for _, src := range m.CardSources() {
		c.On(m.Cards.Handler(src), src.Events()...)
	}
	return c
}
