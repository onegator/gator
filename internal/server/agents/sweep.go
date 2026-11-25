package agents

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

// sweepStore is what the sweeper reads and writes. *db.Queries is one.
type sweepStore interface {
	ListStaleTurns(ctx context.Context, before pgtype.Timestamptz) ([]db.AgentTurn, error)
	ListWaitingTurns(ctx context.Context, before pgtype.Timestamptz) ([]db.AgentTurn, error)
	ClaimTurn(ctx context.Context, arg db.ClaimTurnParams) (db.AgentTurn, error)
	EndClaimedTurn(ctx context.Context, arg db.EndClaimedTurnParams) (db.AgentTurn, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	GetConversation(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
}

// Sweeper ends what a start left hanging. A turn that never got its job (every delivery of its
// mention failed) ends failed and says so; an answer whose job no runner has taken says it is
// waiting, instead of reading for ever.
type Sweeper struct {
	store sweepStore
	voice speaker
	log   *slog.Logger
	// StaleAfter is how long a turn may stay without a job; WaitAfter how long a job may wait
	// for a runner before its answer says so.
	StaleAfter, WaitAfter time.Duration
	now                   func() time.Time
}

// NewSweeper builds it with five minutes for both.
func NewSweeper(store sweepStore, voice speaker, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{store: store, voice: voice, log: log, StaleAfter: 5 * time.Minute, WaitAfter: 5 * time.Minute, now: time.Now}
}

// Sweep looks once.
func (s *Sweeper) Sweep(ctx context.Context) error {
	stale, err := s.store.ListStaleTurns(ctx, ts(s.now().Add(-s.StaleAfter)))
	if err != nil {
		return err
	}
	for _, t := range stale {
		if err := s.endStale(ctx, t); err != nil {
			return err
		}
	}
	waiting, err := s.store.ListWaitingTurns(ctx, ts(s.now().Add(-s.WaitAfter)))
	if err != nil {
		return err
	}
	for _, t := range waiting {
		agent, err := s.store.GetAgent(ctx, t.AgentID)
		if err != nil || !t.ReplyMessageID.Valid {
			continue
		}
		if _, _, err := s.voice.Rewrite(ctx, t.ReplyMessageID, Waiting(agent), conversations.StatusStreaming); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sweeper) endStale(ctx context.Context, t db.AgentTurn) error {
	claimed, err := s.store.ClaimTurn(ctx, db.ClaimTurnParams{ID: t.ID, ClaimTtl: interval(ClaimTTL)})
	if err != nil {
		return ignoreGone(err) // somebody is starting it after all
	}
	if _, err := s.store.EndClaimedTurn(ctx, db.EndClaimedTurnParams{ID: claimed.ID, Status: TurnFailed}); err != nil {
		return ignoreGone(err)
	}
	agent, err := s.store.GetAgent(ctx, t.AgentID)
	if err != nil {
		return err
	}
	say := fmt.Sprintf("@%s could not start answering here; ask again.", agent.Handle)
	if t.ReplyMessageID.Valid {
		_, _, err = s.voice.Rewrite(ctx, t.ReplyMessageID, say, conversations.StatusFailed)
		return err
	}
	conv, err := s.store.GetConversation(ctx, t.ConversationID)
	if err != nil {
		return ignoreGone(err)
	}
	_, err = s.voice.Announce(ctx, conv, say)
	return err
}

// Run sweeps every interval until ctx ends.
func (s *Sweeper) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("sweeping agents' turns", "err", err)
			}
		}
	}
}

// Waiting is what an answer says while no runner takes its job.
func Waiting(a db.Agent) string {
	return fmt.Sprintf("@%s is waiting for a runner to take this; it answers as soon as one does.", a.Handle)
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
