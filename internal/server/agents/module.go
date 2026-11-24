package agents

import (
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// cursorName is this package's row in event_cursors: agents answer at their own pace, and a slow
// start never holds up the cards of the conversations consumer.
const cursorName = "agents"

// Deps are what the package needs from the rest of the server.
type Deps struct {
	Conversations *conversations.Module
	Process       *process.Service
	// Jobs queues the jobs that do turns (runners.Manager).
	Jobs jobQueue
	// Now is the clock of the daily budget; nil is time.Now.
	Now func() time.Time
}

// Module is the package's services wired to one database, for the server to hand out.
type Module struct {
	Dispatcher *Dispatcher
	Starter    *Starter
	Voice      *conversations.Voice

	pool *pgxpool.Pool
}

// New wires the services. Conversations must have Tasks (asks need them).
func New(pool *pgxpool.Pool, deps Deps) *Module {
	q := db.New(pool)
	voice := conversations.NewVoice(pool)
	chat := deps.Conversations
	starter := NewStarter(q, chat.Threads, chat.Asks, voice, deps.Jobs, nil)
	guards := Guards{
		Paused,
		Writers{Access: chat.Access},
		HopLimit{Max: MaxHops},
		NoRepeat,
		ChainBudget{Spent: q, Policies: deps.Process, Factor: DefaultChainFactor},
		DailyBudget{Budgets: deps.Process, Now: deps.Now},
	}
	return &Module{
		Dispatcher: NewDispatcher(q, guards, starter),
		Starter:    starter,
		Voice:      voice,
		pool:       pool,
	}
}

// Consumer is the outbox consumer that has agents answer. Run it with go consumer.Run(ctx).
func (m *Module) Consumer(hub *events.Hub, log *slog.Logger, poll time.Duration) *conversations.Consumer {
	c := conversations.NewCursorConsumer(cursorName, m.pool, hub, log, poll)
	c.On(m.Dispatcher, m.Dispatcher.Events()...)
	return c
}
