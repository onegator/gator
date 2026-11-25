package agents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/store/db"
)

// steerStore is what the steerer reads. *db.Queries is one.
type steerStore interface {
	ListActiveTurnsIn(ctx context.Context, conversationID pgtype.UUID) ([]db.AgentTurn, error)
	GetUser(ctx context.Context, id pgtype.UUID) (db.User, error)
}

// jobSteerer forwards a correction to a running job. runners.Manager is one.
type jobSteerer interface {
	SteerJob(ctx context.Context, jobID pgtype.UUID, message string) error
}

// waitAnswerer refuses, with a person's message as the reason, what an agent waits for them on.
// approvals.Service is one.
type waitAnswerer interface {
	DenyByMessage(ctx context.Context, job pgtype.UUID, author pgtype.UUID, body string) (int, error)
}

// Steerer takes a person's message in a conversation where an agent is still answering as a
// correction of that answer (the job's steer, resumed in the same session) rather than as a new
// question: a message that asks no agent steers every agent answering there, one that names the
// answering agent steers it, and the agents it names that are not answering are asked as ever.
// A job not yet running needs no steer: its agent reads the conversation as it is when it starts.
type Steerer struct {
	store steerStore
	jobs  jobSteerer
	voice announcer
	log   *slog.Logger
	// waits, when set, has the message refuse what the steered agent waits for a person on (a tool
	// approval, a question), with the message as the reason: what a person writes while the agent
	// waits is their answer, and it steers too.
	waits waitAnswerer
}

// announcer says something as Gator in a conversation. conversations.Voice is one.
type announcer interface {
	Announce(ctx context.Context, conv db.Conversation, body string) (db.Message, error)
}

// NewSteerer builds it.
func NewSteerer(store steerStore, jobs jobSteerer, voice announcer, log *slog.Logger) *Steerer {
	if log == nil {
		log = slog.Default()
	}
	return &Steerer{store: store, jobs: jobs, voice: voice, log: log}
}

// Intercept implements Interceptor.
func (s *Steerer) Intercept(ctx context.Context, msg db.Message, conv db.Conversation, asked []db.Agent) ([]db.Agent, error) {
	if msg.AuthorKind != conversations.AuthorUser {
		return asked, nil // agents delegate; only a person corrects
	}
	active, err := s.store.ListActiveTurnsIn(ctx, conv.ID)
	if err != nil || len(active) == 0 {
		return asked, err
	}
	who := "A person"
	if u, err := s.store.GetUser(ctx, msg.AuthorID); err == nil && u.Name != "" {
		who = u.Name
	}
	steered := map[[16]byte]bool{}
	for _, t := range active {
		if !t.JobID.Valid {
			continue // a turn that never got its job answers nothing: asking again asks anew
		}
		named := slices.ContainsFunc(asked, func(a db.Agent) bool { return a.ID == t.AgentID })
		if len(asked) > 0 && !named {
			continue
		}
		steered[t.AgentID.Bytes] = true
		if s.waits != nil {
			if _, err := s.waits.DenyByMessage(ctx, t.JobID, msg.AuthorID, msg.Body); err != nil {
				return nil, err
			}
		}
		err := s.jobs.SteerJob(ctx, t.JobID, fmt.Sprintf("%s says in the conversation:\n\n%s", who, msg.Body))
		switch {
		case err == nil, errors.Is(err, runners.ErrJobNotActive), errors.Is(err, pgx.ErrNoRows):
			// steered, or not started yet (it will read this), or gone
		case errors.Is(err, runners.ErrRunnerOffline):
			// Not steered: say so, rather than let the person think it was heard. The job goes back
			// to the queue when its lease runs out, and its next attempt reads this message.
			if _, err := s.voice.Announce(ctx, conv, "The runner answering here is offline, so this did not reach the agent now; it reads it when its work starts again."); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}
	return slices.DeleteFunc(asked, func(a db.Agent) bool { return steered[a.ID.Bytes] }), nil
}
