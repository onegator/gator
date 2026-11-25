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
}

// Deps are what the Brain needs from the rest of the server.
type Deps struct {
	// Conversations answers who may read a conversation (conversations.Access); nil: no proposal
	// from a private conversation is readable by anyone until approved.
	Conversations ConversationAccess
	// Searcher finds candidates; nil: Postgres full-text search.
	Searcher Searcher
	// Effects are more consequences of approving an entry (see Effect).
	Effects []Effect
}

// New wires the Brain.
func New(pool *pgxpool.Pool, deps Deps) *Module {
	q := db.New(pool)
	policy := NewPolicy(deps.Conversations)
	searcher := deps.Searcher
	if searcher == nil {
		searcher = NewFTS(q)
	}
	return &Module{
		Policy:  policy,
		Readers: NewReaders(q),
		Service: NewService(PoolTx(pool), q, policy, DefaultSources(), deps.Effects...),
		Finder:  NewFinder(searcher, policy),
	}
}
