package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/store/db"
)

var (
	// ErrNotFound answers an agent or a turn that does not exist, or that the caller may not see.
	ErrNotFound = errors.New("not found")
	// ErrForbidden answers something the caller may see but not do.
	ErrForbidden = errors.New("not allowed")
	// ErrConflict answers a request that clashes with how things are: a turn that ended.
	ErrConflict = errors.New("conflict")
	// ErrInvalid answers a request that cannot be carried out as asked.
	ErrInvalid = errors.New("invalid")
)

// jobStopper stops a job. runners.Manager is one.
type jobStopper interface {
	StopJob(ctx context.Context, jobID pgtype.UUID, reason string) error
}

// conversationRights answers what a caller may do in a conversation. conversations.Access is one.
type conversationRights interface {
	RightsByID(ctx context.Context, id pgtype.UUID, p auth.Principal) (db.Conversation, conversations.Rights, error)
}

// controlStore is what Control reads and writes. *db.Queries is one.
type controlStore interface {
	GetTurn(ctx context.Context, id pgtype.UUID) (db.AgentTurn, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	SetTurnStatus(ctx context.Context, arg db.SetTurnStatusParams) (db.AgentTurn, error)
}

// Control is what people do to turns from a conversation. Stop is the button under an answer
// being written.
type Control struct {
	store  controlStore
	rights conversationRights
	jobs   jobStopper
	voice  rewriter
}

// NewControl builds it.
func NewControl(store controlStore, rights conversationRights, jobs jobStopper, voice rewriter) *Control {
	return &Control{store: store, rights: rights, jobs: jobs, voice: voice}
}

// Turn answers a turn the caller may read: one in a conversation they may read.
func (c *Control) Turn(ctx context.Context, p auth.Principal, id pgtype.UUID) (db.AgentTurn, conversations.Rights, error) {
	turn, err := c.store.GetTurn(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTurn{}, conversations.Rights{}, ErrNotFound
	}
	if err != nil {
		return db.AgentTurn{}, conversations.Rights{}, err
	}
	_, rights, err := c.rights.RightsByID(ctx, turn.ConversationID, p)
	if err != nil {
		return db.AgentTurn{}, conversations.Rights{}, err
	}
	if !rights.Read {
		return db.AgentTurn{}, conversations.Rights{}, ErrNotFound
	}
	return turn, rights, nil
}

// Stop ends a turn that is still answering, for anyone who may write where it answers: its job is
// stopped (the tracker ends the turn when the job says it stopped), or a turn with no job yet
// ends at once.
func (c *Control) Stop(ctx context.Context, p auth.Principal, id pgtype.UUID, reason string) (db.AgentTurn, error) {
	turn, rights, err := c.Turn(ctx, p, id)
	if err != nil {
		return db.AgentTurn{}, err
	}
	if !rights.Write {
		return db.AgentTurn{}, ErrForbidden
	}
	if turn.Status != TurnQueued && turn.Status != TurnRunning {
		return turn, fmt.Errorf("%w: this turn has ended (%s)", ErrConflict, turn.Status)
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "by " + orText(p.Name, "a person")
	}
	if turn.JobID.Valid {
		err := c.jobs.StopJob(ctx, turn.JobID, reason)
		if errors.Is(err, runners.ErrJobFinished) {
			return turn, fmt.Errorf("%w: this turn has just ended", ErrConflict)
		}
		return turn, err
	}
	agent, err := c.store.GetAgent(ctx, turn.AgentID)
	if err != nil {
		return db.AgentTurn{}, err
	}
	ended, err := c.store.SetTurnStatus(ctx, db.SetTurnStatusParams{ID: turn.ID, Status: TurnStopped})
	if errors.Is(err, pgx.ErrNoRows) {
		return turn, fmt.Errorf("%w: this turn has just ended", ErrConflict)
	}
	if err != nil {
		return db.AgentTurn{}, err
	}
	if turn.ReplyMessageID.Valid {
		if _, _, err := c.voice.Rewrite(ctx, turn.ReplyMessageID, stoppedNote(agent, reason), conversations.StatusFinal); err != nil {
			return ended, err
		}
	}
	return ended, nil
}
