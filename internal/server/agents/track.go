package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/store/db"
)

// trackStore is what the tracker reads and writes. *db.Queries is one.
type trackStore interface {
	GetJob(ctx context.Context, id pgtype.UUID) (db.Job, error)
	GetTurnByJob(ctx context.Context, jobID pgtype.UUID) (db.AgentTurn, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	GetMessage(ctx context.Context, id pgtype.UUID) (db.Message, error)
	SetTurnStatus(ctx context.Context, arg db.SetTurnStatusParams) (db.AgentTurn, error)
}

// TurnTracker keeps a turn in step with its job: running once the job runs, and at its end done,
// failed or stopped, with the agent's message final — its answer, or why there is none. It reads
// the outbox (job.started, job.finished), so no end is missed.
type TurnTracker struct {
	store trackStore
	voice rewriter
}

// NewTurnTracker builds it.
func NewTurnTracker(store trackStore, voice rewriter) *TurnTracker {
	return &TurnTracker{store: store, voice: voice}
}

// Events are the outbox events it reads.
func (*TurnTracker) Events() []string { return []string{"job.started", "job.finished"} }

// Handle implements conversations.Handler.
func (t *TurnTracker) Handle(ctx context.Context, e db.EventsAfterRow) error {
	job, err := t.store.GetJob(ctx, e.AggregateID)
	if err != nil {
		return ignoreGone(err)
	}
	if job.Kind != runners.KindReply {
		return nil
	}
	turn, err := t.store.GetTurnByJob(ctx, job.ID)
	if err != nil {
		return ignoreGone(err)
	}
	if e.Type == "job.started" {
		return t.status(ctx, turn, TurnRunning)
	}
	if !terminalJob(job.Status) {
		return nil // a finished event of a job requeued since
	}
	agent, err := t.store.GetAgent(ctx, turn.AgentID)
	if err != nil {
		return err
	}
	status := map[string]string{"done": TurnDone, "failed": TurnFailed, "stopped": TurnStopped}[job.Status]
	if err := t.status(ctx, turn, status); err != nil {
		return err
	}
	if !turn.ReplyMessageID.Valid {
		return nil
	}
	said, err := t.store.GetMessage(ctx, turn.ReplyMessageID)
	if err != nil {
		return ignoreGone(err)
	}
	body, msgStatus := Ending(agent, job, said)
	_, _, err = t.voice.Rewrite(ctx, turn.ReplyMessageID, body, msgStatus)
	return err
}

func (t *TurnTracker) status(ctx context.Context, turn db.AgentTurn, status string) error {
	_, err := t.store.SetTurnStatus(ctx, db.SetTurnStatusParams{ID: turn.ID, Status: status})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // ended already: an ended turn stays as it ended
	}
	return err
}

// Ending is what an agent's message says when its job ends, and whether that is an answer
// (final) or the lack of one (failed). A done job's answer is its summary, which the runner
// already took the digest out of; a stopped one keeps what it had said, and says it stopped.
func Ending(agent db.Agent, job db.Job, said db.Message) (string, string) {
	var receipt struct {
		Summary string `json:"summary"`
	}
	_ = json.Unmarshal(job.Receipt, &receipt)
	reason := ""
	if job.StopReason != nil {
		reason = *job.StopReason
	}
	partial := ""
	if said.Status == conversations.StatusStreaming && said.Body != Reading(agent) {
		partial = strings.TrimSpace(said.Body)
	}
	switch job.Status {
	case "done":
		if s := strings.TrimSpace(receipt.Summary); s != "" {
			return s, conversations.StatusFinal
		}
		return fmt.Sprintf("_@%s finished without an answer._", agent.Handle), conversations.StatusFinal
	case "stopped":
		note := stoppedNote(agent, reason)
		if partial != "" {
			return partial + "\n\n" + note, conversations.StatusFinal
		}
		return note, conversations.StatusFinal
	}
	return fmt.Sprintf("@%s could not answer: %s", agent.Handle, orText(reason, "its job failed")), conversations.StatusFailed
}

func terminalJob(status string) bool {
	return status == "done" || status == "failed" || status == "stopped"
}

func orText(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

// stoppedNote is how an answer that was stopped ends: who stopped it, if the reason says.
func stoppedNote(agent db.Agent, reason string) string {
	reason = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(reason), "stopped:"))
	if reason == "" || reason == "stopped by request" {
		return fmt.Sprintf("_@%s stopped._", agent.Handle)
	}
	return fmt.Sprintf("_@%s stopped (%s)._", agent.Handle, reason)
}
