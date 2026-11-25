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
	// Jobs queues, steers and stops the jobs that do turns (runners.Manager).
	Jobs interface {
		jobQueue
		jobSteerer
		jobStopper
	}
	// Roles answers people's roles in projects (auth.Authorizer).
	Roles roleResolver
	// Now is the clock of the daily budget; nil is time.Now.
	Now func() time.Time
	Log *slog.Logger
}

// Module is the package's services wired to one database, for the server to hand out.
type Module struct {
	Dispatcher *Dispatcher
	Starter    *Starter
	Voice      *conversations.Voice
	// Briefer tells a reply's agent its conversation: runners.Config.Briefer.
	Briefer   *Briefer
	Summaries *conversations.Summaries
	// Tracker keeps turns in step with their jobs.
	Tracker *TurnTracker
	// Control is what people do to turns: stop them.
	Control *Control
	// Talk is what agents say from inside their jobs.
	Talk *Talk
	// Consent asks a person to let a work agent work, and lets it.
	Consent *Consent
	// Directory keeps the agents themselves.
	Directory *Directory
	// Curations queues the curator's reading of conversations (curator.Jobs).
	Curations CurationQueue

	pool *pgxpool.Pool
}

// New wires the services. Conversations must have Tasks (asks need them).
func New(pool *pgxpool.Pool, deps Deps) *Module {
	q := db.New(pool)
	voice := conversations.NewVoice(pool)
	chat := deps.Conversations
	starter := NewStarter(q, chat.Threads, chat.Asks, voice, deps.Jobs, NewConsentCards(q, voice))
	guards := Guards{
		Paused,
		Writers{Access: chat.Access},
		HopLimit{Max: MaxHops},
		NoRepeat,
		FanOut{Turns: q, PerTurn: MaxDelegationsPerTurn, PerChain: MaxTurnsPerChain},
		ChainBudget{Spent: q, Policies: deps.Process, Tasks: q, Factor: DefaultChainFactor},
		AgentDailyBudget{Spent: q, Now: deps.Now},
		OutsideBudget{Spent: q, Default: DefaultOutsideDailyBudget, Now: deps.Now},
		DailyBudget{Budgets: deps.Process, Now: deps.Now},
	}
	summaries := conversations.NewSummaries(pool, summaryQueue{jobs: deps.Jobs})
	consent := NewConsent(q, voice, chat.Access, deps.Roles, deps.Process, guards, starter)
	return &Module{
		Consent:    consent,
		Directory:  NewDirectory(q, deps.Roles, chat.Access),
		Dispatcher: NewDispatcher(q, guards, starter, NewSteerer(q, deps.Jobs, voice, deps.Log)),
		Control:    NewControl(q, chat.Access, deps.Jobs, voice),
		Starter:    starter,
		Voice:      voice,
		Briefer:    NewBriefer(q, chat.Packer, summaries, deps.Log),
		Summaries:  summaries,
		Tracker:    NewTurnTracker(q, voice),
		Talk:       NewTalk(q, chat.Threads, voice, chat.Packer, chat.Proposals, chat.Access),
		Curations:  CurationQueue{jobs: deps.Jobs},
		pool:       pool,
	}
}

// AnswerElsewhere has the turns of agents of kind answered by e instead of a runner's job: started
// by the Starter, stopped by Control.
func (m *Module) AnswerElsewhere(kind string, e Elsewhere) {
	m.Starter.AnswerElsewhere(kind, e)
	m.Control.answerElsewhere(kind, e)
}

// Consumer is the outbox consumer that has agents answer. Run it with go consumer.Run(ctx).
func (m *Module) Consumer(hub *events.Hub, log *slog.Logger, poll time.Duration) *conversations.Consumer {
	c := conversations.NewCursorConsumer(cursorName, m.pool, hub, log, poll)
	c.On(m.Dispatcher, m.Dispatcher.Events()...)
	c.On(m.Summaries.Handler(), "job.finished")
	c.On(m.Tracker, m.Tracker.Events()...)
	return c
}

// Sweeper ends what starts left hanging. Run it with go sweeper.Run(ctx, time.Minute).
func (m *Module) Sweeper(log *slog.Logger) *Sweeper {
	return NewSweeper(db.New(m.pool), m.Voice, log)
}

// Streamer writes replies into their messages as agents write them, from the hub's job output.
// Run it with go streamer.Run(ctx).
func (m *Module) Streamer(hub *events.Hub, log *slog.Logger, every time.Duration) *Streamer {
	return NewStreamer(hub, db.New(m.pool), m.Voice, m.pool, every, log)
}
