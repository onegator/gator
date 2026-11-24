package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// Statuses of a turn (agent_turns.status).
const (
	TurnQueued  = "queued"
	TurnRunning = "running"
	TurnDone    = "done"
	TurnFailed  = "failed"
	TurnRefused = "refused"
	TurnStopped = "stopped"
)

// replyHomes answers where an answer to a message goes. conversations.Threads is one.
type replyHomes interface {
	Under(ctx context.Context, msg db.Message) (db.Conversation, error)
}

// askFinder answers the task an answer in a conversation belongs to. conversations.Asks is one.
type askFinder interface {
	For(ctx context.Context, home db.Conversation, trigger db.Message, by process.Actor) (db.Task, error)
}

// speaker says things in a conversation as an agent or as Gator. conversations.Voice is one.
type speaker interface {
	Say(ctx context.Context, conv db.Conversation, s conversations.Speech) (db.Message, error)
	Rewrite(ctx context.Context, message pgtype.UUID, body, status string) (db.Message, bool, error)
	Announce(ctx context.Context, conv db.Conversation, body string) (db.Message, error)
}

// jobQueue queues the job that does a turn. runners.Manager is one.
type jobQueue interface {
	CreateJob(ctx context.Context, taskID pgtype.UUID, in runners.NewJob) (db.Job, error)
}

// ConsentAsker asks a person whether a work agent that answered in chat may work on the task.
type ConsentAsker interface {
	AskConsent(ctx context.Context, turn db.AgentTurn, home db.Conversation, task db.Task) error
}

// turnLocker runs fn with the turn held, so that nobody else starts it meanwhile. Locker is one.
type turnLocker interface {
	WithTurn(ctx context.Context, id pgtype.UUID, fn func(turn db.AgentTurn, q turnStore) error) error
}

// turnStore is what the starter writes. *db.Queries is one.
type turnStore interface {
	InsertTurn(ctx context.Context, arg db.InsertTurnParams) (db.AgentTurn, error)
	GetTurnByTrigger(ctx context.Context, arg db.GetTurnByTriggerParams) (db.AgentTurn, error)
	SetTurnReply(ctx context.Context, arg db.SetTurnReplyParams) (db.AgentTurn, error)
	SetTurnJob(ctx context.Context, arg db.SetTurnJobParams) (db.AgentTurn, error)
	SetTurnStatus(ctx context.Context, arg db.SetTurnStatusParams) (db.AgentTurn, error)
}

// Starter turns a request that passed the guards into a turn: the answer's place in the
// conversation, the task it belongs to, the agent's message saying it is on it, and the job
// that writes the answer. Every step can be repeated, so a redelivered mention, or one whose
// first try failed halfway, finishes what the first began and starts nothing twice.
type Starter struct {
	turns   turnStore
	locks   turnLocker
	homes   replyHomes
	asks    askFinder
	voice   speaker
	jobs    jobQueue
	consent ConsentAsker // optional: without it a work agent without consent only chats
}

// NewStarter builds it.
func NewStarter(turns turnStore, locks turnLocker, homes replyHomes, asks askFinder, voice speaker, jobs jobQueue, consent ConsentAsker) *Starter {
	return &Starter{turns: turns, locks: locks, homes: homes, asks: asks, voice: voice, jobs: jobs, consent: consent}
}

// maxInstruction bounds the question a job carries as its instruction; the conversation itself
// reaches the agent in its prompt.
const maxInstruction = 2000

// Start starts the turn r asks for, or finishes starting it.
func (s *Starter) Start(ctx context.Context, r Request) error {
	home, err := s.homes.Under(ctx, r.Trigger)
	if err != nil {
		return err
	}
	opened, _, err := s.open(ctx, r, home, nil)
	if err != nil {
		return err
	}
	return s.locks.WithTurn(ctx, opened.ID, func(turn db.AgentTurn, turns turnStore) error {
		if turn.Status != TurnQueued || turn.JobID.Valid {
			return nil // started already, or ended
		}
		return s.start(ctx, r, home, turn, turns)
	})
}

// start does the work of Start with the turn held: turns writes under that hold.
func (s *Starter) start(ctx context.Context, r Request, home db.Conversation, turn db.AgentTurn, turns turnStore) error {
	task, err := s.asks.For(ctx, home, r.Trigger, actorOf(r))
	if errors.Is(err, conversations.ErrInvalid) || errors.Is(err, conversations.ErrConflict) {
		return s.end(ctx, turns, turn, home, TurnRefused, &Refusal{Code: RefusedNoTask, Say: plain(err)})
	}
	if err != nil {
		return err
	}
	grant := TierFor(r.Agent, task)
	if !turn.ReplyMessageID.Valid {
		msg, err := s.voice.Say(ctx, home, conversations.Speech{Agent: r.Agent.ID, Turn: turn.ID,
			Body: Reading(r.Agent), Status: conversations.StatusStreaming})
		if err != nil {
			return err
		}
		if turn, err = turns.SetTurnReply(ctx, db.SetTurnReplyParams{ID: turn.ID, ReplyMessageID: msg.ID}); err != nil {
			return err
		}
	}
	job, err := s.jobs.CreateJob(ctx, task.ID, runners.NewJob{
		Agent: r.Agent.ID, Turn: turn.ID, Kind: runners.KindReply, Tier: grant.Tier, Role: roleOf(r.Agent),
		Instruction: clip(r.Trigger.Body, maxInstruction), CreatedBy: actorOf(r),
	})
	if errors.Is(err, runners.ErrBudgetExceeded) {
		return s.end(ctx, turns, turn, home, TurnRefused, &Refusal{Code: RefusedBudget, Say: fmt.Sprintf("@%s does not answer today: %s", r.Agent.Handle, plain(err))})
	}
	if errors.Is(err, runners.ErrInvalidJob) || errors.Is(err, runners.ErrAgentsNotAllowed) {
		return s.end(ctx, turns, turn, home, TurnFailed, &Refusal{Say: fmt.Sprintf("@%s could not start: %s", r.Agent.Handle, plain(err))})
	}
	if err != nil {
		return err
	}
	if turn, err = turns.SetTurnJob(ctx, db.SetTurnJobParams{ID: turn.ID, JobID: job.ID, Tier: grant.Tier}); err != nil {
		return err
	}
	if grant.NeedsConsent && s.consent != nil {
		return s.consent.AskConsent(ctx, turn, home, task)
	}
	return nil
}

// Refuse records a refused turn and says why where the answer would have gone, once.
func (s *Starter) Refuse(ctx context.Context, r Request, no Refusal) error {
	home, err := s.homes.Under(ctx, r.Trigger)
	if err != nil {
		return err
	}
	_, fresh, err := s.open(ctx, r, home, &no)
	if err != nil || !fresh {
		return err
	}
	_, err = s.voice.Announce(ctx, home, no.Say)
	return err
}

// open records the turn, or finds the one this message already asked for. fresh says this call
// wrote it.
func (s *Starter) open(ctx context.Context, r Request, home db.Conversation, refused *Refusal) (db.AgentTurn, bool, error) {
	chain, _ := r.Chain()
	in := db.InsertTurnParams{
		AgentID: r.Agent.ID, ProjectID: r.Project, ConversationID: home.ID, TriggerMessageID: r.Trigger.ID,
		RequestedByKind: r.RequestedByKind, RequestedBy: r.RequestedBy, OnBehalfOf: r.OnBehalfOf(),
		ChainID: chain, Hop: int16(r.Hop()), Tier: TierChat, Status: TurnQueued,
	}
	if r.Parent != nil {
		in.ParentTurnID = r.Parent.ID
	}
	if refused != nil {
		in.Status, in.Refusal = TurnRefused, &refused.Code
	}
	turn, err := s.turns.InsertTurn(ctx, in)
	if err == nil {
		return turn, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTurn{}, false, err
	}
	turn, err = s.turns.GetTurnByTrigger(ctx, db.GetTurnByTriggerParams{AgentID: r.Agent.ID, TriggerMessageID: r.Trigger.ID})
	return turn, false, err
}

// end closes a turn that could not run, and says why: in the agent's message when it has one,
// else as a line from Gator.
func (s *Starter) end(ctx context.Context, turns turnStore, turn db.AgentTurn, home db.Conversation, status string, why *Refusal) error {
	var code *string
	if status == TurnRefused {
		code = &why.Code
	}
	if _, err := turns.SetTurnStatus(ctx, db.SetTurnStatusParams{ID: turn.ID, Status: status, Refusal: code}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if turn.ReplyMessageID.Valid {
		_, _, err := s.voice.Rewrite(ctx, turn.ReplyMessageID, why.Say, conversations.StatusFailed)
		return err
	}
	_, err := s.voice.Announce(ctx, home, why.Say)
	return err
}

// actorOf is who a turn's job is created by: the person who asked, or Gator for an agent.
func actorOf(r Request) process.Actor {
	if r.RequestedByKind == RequestedByUser {
		return process.Actor{Kind: process.ActorUser, ID: r.RequestedBy}
	}
	return process.Actor{Kind: process.ActorSystem}
}

// plain is an error as a sentence for a conversation, without the package prefixes.
func plain(err error) string {
	s := err.Error()
	for _, p := range []string{"invalid: ", "conflict: ", "invalid job: "} {
		s = strings.TrimPrefix(s, p)
	}
	return s
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// Locker holds turns in a transaction of their own (turnLocker).
type Locker struct{ Pool *pgxpool.Pool }

// WithTurn implements turnLocker: fn runs with the turn's row held until it returns, and what fn
// writes through q commits with the release.
func (l Locker) WithTurn(ctx context.Context, id pgtype.UUID, fn func(turn db.AgentTurn, q turnStore) error) error {
	return store.InTx(ctx, l.Pool, func(q *db.Queries) error {
		turn, err := q.LockTurn(ctx, id)
		if err != nil {
			return err
		}
		return fn(turn, q)
	})
}

// Reading is what an agent's message says from the moment it is asked until it writes: the
// session takes seconds to start, and nobody should wonder whether it heard.
func Reading(a db.Agent) string { return fmt.Sprintf("@%s is reading the conversation…", a.Handle) }
