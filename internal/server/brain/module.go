package brain

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store/db"
)

// Module is the Brain's pieces wired to one database, for the server to hand out.
type Module struct {
	Policy  Policy
	Readers Readers
	Service *Service
	Finder  Finder
	// Legacy is the product context's and the knowledge shelves' old doors, over the Brain.
	Legacy Legacy
	// Plugins is the door plugins propose through (brain.propose).
	Plugins *PluginProposals
	// Focus works out people's focus from their work.
	Focus *Focus
}

// Deps are what the Brain needs from the rest of the server.
type Deps struct {
	// Conversations answers who may read a conversation (conversations.Access); nil: no proposal
	// from a private conversation is readable by anyone until approved.
	Conversations ConversationAccess
	// Searcher finds candidates; nil: Postgres full-text search.
	Searcher Searcher
	// Hooks are more consequences of proposing and approving entries (see Effect).
	Hooks Hooks
}

// New wires the Brain.
func New(pool *pgxpool.Pool, deps Deps) *Module {
	q := db.New(pool)
	policy := NewPolicy(deps.Conversations)
	searcher := deps.Searcher
	if searcher == nil {
		searcher = NewFTS(q)
	}
	// A proposed decision is checked against the approved ones it may replace, and approving it
	// supersedes them.
	contradictions := NewContradictions(searcher)
	hooks := Hooks{
		Proposed: append([]Effect{contradictions.Link}, deps.Hooks.Proposed...),
		Approved: append([]Effect{SupersedeContradicted}, deps.Hooks.Approved...),
	}
	svc := NewService(PoolTx(pool), q, policy, DefaultSources(), hooks)
	return &Module{
		Policy:  policy,
		Readers: NewReaders(q),
		Service: svc,
		Finder:  NewFinder(searcher, policy),
		Legacy:  NewLegacy(q, policy),
		Plugins: NewPluginProposals(q, svc),
		Focus:   NewFocus(q, svc, nil),
	}
}
