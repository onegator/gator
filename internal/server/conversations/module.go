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
	// Tasks creates tasks and promotes asks (process.Service). Nil: messages cannot be made into
	// tasks, and nobody's conversation with an agent opens one.
	Tasks Tasks
	// Blobs keeps uploaded files (BlobStoreFromEnv). Nil: no attachments.
	Blobs BlobStore
	// CardSources are more kinds of cards (a plugin's pull requests, an incident), written by
	// the same projector after the built-in ones. A new kind of card is one more source here.
	CardSources []CardSource
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
	// Packer tells an agent the conversation it answers in.
	Packer *Packer
	// Promoter, Asks and Proposals are nil without Deps.Tasks.
	Promoter  *Promoter
	Asks      *Asks
	Proposals *Proposals
	// Attachments is nil without Deps.Blobs.
	Attachments *Attachments
	// Bridged writes what messaging bridges carry in.
	Bridged *Bridged
	// Queue is what people asked to be done once an agent's turn ends.
	Queue *Queue

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
		Packer:    NewPacker(db.New(pool), 0),
		Bridged:   NewBridged(pool),
		Queue:     NewQueue(pool, access),
		pool:      pool,
		deps:      deps,
	}
	if deps.Tasks != nil {
		m.Promoter = NewPromoter(pool, access, deps.Roles, deps.Tasks, deps.Tasks, threads)
		m.Asks = NewAsks(pool, deps.Tasks)
		m.Proposals = NewProposals(pool, access, deps.Roles, deps.Tasks)
	}
	if deps.Blobs != nil {
		m.Attachments = NewAttachments(pool, access, deps.Blobs)
		m.Messages.attach = m.Attachments
	}
	return m
}

// CardSources are the kinds of cards work leaves in conversations: each task's own card (which
// opens its thread), the decision its gate waits for, its jobs and what its phases produced, then
// whatever Deps.CardSources adds.
func (m *Module) CardSources() []CardSource {
	q := db.New(m.pool)
	var kinds hiddenKinds
	if m.deps.Tasks != nil {
		kinds = m.deps.Tasks
	}
	sources := []CardSource{taskCards{tasks: q, kinds: kinds}, jobCards{jobs: q}, artifactCards{artifacts: q}}
	if m.deps.Decisions != nil {
		sources = append(sources, decisionCards{tasks: q, decisions: m.deps.Decisions, index: m.Cards})
	}
	return append(sources, m.deps.CardSources...)
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
