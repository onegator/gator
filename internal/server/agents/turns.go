package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/runners"
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
// Asking again is asking once: a card already there is left as it is.
type ConsentAsker interface {
	AskConsent(ctx context.Context, turn db.AgentTurn, home db.Conversation, task db.Task) error
}

// turnStore is what the starter reads and writes. *db.Queries is one.
type turnStore interface {
	InsertTurn(ctx context.Context, arg db.InsertTurnParams) (db.AgentTurn, error)
	GetTurnByTrigger(ctx context.Context, arg db.GetTurnByTriggerParams) (db.AgentTurn, error)
	GetTurn(ctx context.Context, id pgtype.UUID) (db.AgentTurn, error)
	ClaimTurn(ctx context.Context, arg db.ClaimTurnParams) (db.AgentTurn, error)
	ReleaseTurn(ctx context.Context, id pgtype.UUID) error
	EndClaimedTurn(ctx context.Context, arg db.EndClaimedTurnParams) (db.AgentTurn, error)
	SetTurnReply(ctx context.Context, arg db.SetTurnReplyParams) (db.AgentTurn, error)
	SetTurnJob(ctx context.Context, arg db.SetTurnJobParams) (db.AgentTurn, error)
	SetTurnStatus(ctx context.Context, arg db.SetTurnStatusParams) (db.AgentTurn, error)
	GetJobOfTurn(ctx context.Context, turnID pgtype.UUID) (db.Job, error)
	GetJob(ctx context.Context, id pgtype.UUID) (db.Job, error)
	GetTask(ctx context.Context, id pgtype.UUID) (db.Task, error)
}

// Starter turns a request that passed the guards into a turn: the answer's place in the
// conversation, the task it belongs to, the agent's message saying it is on it, and the job
// that writes the answer. Every step can be repeated, so a redelivered mention, or one whose
// first try died halfway, finishes what the first began and starts nothing twice: the job is
// queued by whoever claims the turn (ClaimTurn), found again by its turn_id, and the answer's
// message by its client id.
type Starter struct {
	turns   turnStore
	homes   replyHomes
	asks    askFinder
	voice   speaker
	jobs    jobQueue
	consent ConsentAsker // optional: without it a work agent without consent only chats
}

// NewStarter builds it.
func NewStarter(turns turnStore, homes replyHomes, asks askFinder, voice speaker, jobs jobQueue, consent ConsentAsker) *Starter {
	return &Starter{turns: turns, homes: homes, asks: asks, voice: voice, jobs: jobs, consent: consent}
}

// ClaimTTL is how long a start may hold a turn before another may take it over.
const ClaimTTL = 2 * time.Minute

// Start starts the turn r asks for, or finishes starting it.
func (s *Starter) Start(ctx context.Context, r Request) error {
	home, err := s.homes.Under(ctx, r.Trigger)
	if err != nil {
		return err
	}
	turn, _, err := s.open(ctx, r, home, nil)
	if err != nil {
		return err
	}
	if !turn.JobID.Valid {
		claimed, err := s.turns.ClaimTurn(ctx, db.ClaimTurnParams{ID: turn.ID, ClaimTtl: interval(ClaimTTL)})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // somebody else is starting it, or it has ended
		}
		if err != nil {
			return err
		}
		if turn, err = s.start(ctx, r, home, claimed); err != nil {
			_ = s.turns.ReleaseTurn(context.WithoutCancel(ctx), claimed.ID) // the next delivery tries again
			return err
		}
	}
	return s.finish(ctx, r, home, turn)
}

// start queues the job of a claimed turn, or finds the one an earlier start queued.
func (s *Starter) start(ctx context.Context, r Request, home db.Conversation, turn db.AgentTurn) (db.AgentTurn, error) {
	if job, err := s.turns.GetJobOfTurn(ctx, turn.ID); err == nil {
		return s.turns.SetTurnJob(ctx, db.SetTurnJobParams{ID: turn.ID, JobID: job.ID, Tier: job.Tier})
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return turn, err
	}
	task, err := s.asks.For(ctx, home, r.Question(), actorOf(r))
	if errors.Is(err, conversations.ErrInvalid) || errors.Is(err, conversations.ErrConflict) {
		return turn, s.end(ctx, turn, home, TurnRefused, &Refusal{Code: RefusedNoTask, Say: plain(err)})
	}
	if err != nil {
		return turn, err
	}
	grant := TierFor(r.Agent, task)
	if !turn.ReplyMessageID.Valid {
		msg, err := s.voice.Say(ctx, home, conversations.Speech{Agent: r.Agent.ID, Turn: turn.ID,
			Body: Reading(r.Agent), Status: conversations.StatusStreaming, ClientID: "turn:" + conversations.UUIDString(turn.ID)})
		if err != nil {
			return turn, err
		}
		if turn, err = s.turns.SetTurnReply(ctx, db.SetTurnReplyParams{ID: turn.ID, ReplyMessageID: msg.ID}); err != nil {
			return turn, err
		}
	}
	job, err := s.jobs.CreateJob(ctx, task.ID, runners.NewJob{
		Agent: r.Agent.ID, Turn: turn.ID, Kind: runners.KindReply, Tier: grant.Tier, Role: roleOf(r.Agent),
		Instruction: "Answer " + conversations.MessageLink(r.Question()), CreatedBy: actorOf(r),
	})
	if errors.Is(err, runners.ErrBudgetExceeded) {
		return turn, s.end(ctx, turn, home, TurnRefused, &Refusal{Code: RefusedBudget, Say: fmt.Sprintf("@%s does not answer today: %s", r.Agent.Handle, plain(err))})
	}
	if errors.Is(err, runners.ErrInvalidJob) || errors.Is(err, runners.ErrAgentsNotAllowed) {
		return turn, s.end(ctx, turn, home, TurnFailed, &Refusal{Say: fmt.Sprintf("@%s could not start: %s", r.Agent.Handle, plain(err))})
	}
	if err != nil {
		return turn, err
	}
	return s.turns.SetTurnJob(ctx, db.SetTurnJobParams{ID: turn.ID, JobID: job.ID, Tier: grant.Tier})
}

// finish does what follows a started turn, as often as it is asked: a work agent answering in chat
// asks for consent.
func (s *Starter) finish(ctx context.Context, r Request, home db.Conversation, turn db.AgentTurn) error {
	if s.consent == nil || !turn.JobID.Valid || turn.Tier != TierChat || r.Agent.Tier != TierWork {
		return nil
	}
	job, err := s.turns.GetJob(ctx, turn.JobID)
	if err != nil {
		return err
	}
	task, err := s.turns.GetTask(ctx, job.TaskID)
	if err != nil {
		return err
	}
	if !TierFor(r.Agent, task).NeedsConsent {
		return nil
	}
	return s.consent.AskConsent(ctx, turn, home, task)
}

// Refuse records a refused turn and says why where the answer would have gone, once.
func (s *Starter) Refuse(ctx context.Context, r Request, no Refusal) error {
	home, err := s.homes.Under(ctx, r.Trigger)
	if err != nil {
		return err
	}
	turn, fresh, err := s.open(ctx, r, home, &no)
	if err != nil {
		return err
	}
	if !fresh {
		// Asked again while it waited without a job, and now refused: it ends refused, and says so,
		// rather than standing in its thread for ever.
		if turn.Status != TurnQueued || turn.JobID.Valid {
			return nil
		}
		claimed, err := s.turns.ClaimTurn(ctx, db.ClaimTurnParams{ID: turn.ID, ClaimTtl: interval(ClaimTTL)})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := s.turns.EndClaimedTurn(ctx, db.EndClaimedTurnParams{ID: claimed.ID, Status: TurnRefused, Refusal: &no.Code}); err != nil {
			return err
		}
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
	if r.EscalatedFrom != nil {
		in.EscalatedFrom = r.EscalatedFrom.ID
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
func (s *Starter) end(ctx context.Context, turn db.AgentTurn, home db.Conversation, status string, why *Refusal) error {
	var code *string
	if status == TurnRefused {
		code = &why.Code
	}
	if _, err := s.turns.SetTurnStatus(ctx, db.SetTurnStatusParams{ID: turn.ID, Status: status, Refusal: code}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
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

// interval is a duration as Postgres reads it.
func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// Reading is what an agent's message says from the moment it is asked until it writes: the
// session takes seconds to start, and nobody should wonder whether it heard.
func Reading(a db.Agent) string { return fmt.Sprintf("@%s is reading the conversation…", a.Handle) }
