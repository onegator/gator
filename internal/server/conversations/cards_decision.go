package conversations

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// DecisionSource answers whether a task waits on a person, by the inbox's rule.
// process.Service is one.
type DecisionSource interface {
	DecisionFor(ctx context.Context, taskID pgtype.UUID) (process.Decision, bool, error)
}

// Decision card states.
const (
	decisionPending  = "pending"
	decisionResolved = "resolved"
)

// decisionCards shows in a task's thread the decision its gate waits for: approving a plan,
// letting outside work in, a failed job. The card's buttons are the task's own endpoints
// (approve, rollback, handoff, allow-agents, admit). When the task stops waiting — approved,
// moved on, closed — the card says it was decided; a later wait is a new card. The inbox stays
// the source of truth: the card shows the same rule for one task.
type decisionCards struct {
	tasks     taskReader
	decisions DecisionSource
	index     CardIndex
}

// Events implements CardSource: whatever can start or end a wait.
func (decisionCards) Events() []string {
	return []string{"task.created", "task.updated", "task.phase_changed", "task.closed", "task.reopened",
		"task.admitted", "task.agents_allowed", "task.requirements_changed", "task.handed_off",
		"gate.approved", "gate.blocked", "gate.unblocked", "gate.check_set",
		"task.job_queued", "task.job_leased", "task.job_started", "task.job_finished", "task.job_requeued",
		"task.job_stalled", "task.job_no_runner"}
}

// Cards implements CardSource.
func (s decisionCards) Cards(ctx context.Context, e db.EventsAfterRow) ([]PlacedCard, error) {
	task, ok, err := taskOf(ctx, s.tasks, e.AggregateID)
	if !ok {
		return nil, err
	}
	d, waiting, err := s.decisions.DecisionFor(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	existing, err := s.index.TaskCards(ctx, task, CardDecision)
	if err != nil {
		return nil, err
	}
	cards := decisionUpdates(d, waiting, existing)
	out := make([]PlacedCard, 0, len(cards))
	for _, c := range cards {
		out = append(out, PlacedCard{Task: task, Card: c})
	}
	return out, nil
}

// decisionUpdates is what the decision cards of a task should say now: the wait there is, if
// any, and every other card still pending marked decided.
func decisionUpdates(d process.Decision, waiting bool, existing []db.Message) []Card {
	var out []Card
	current := ""
	if waiting {
		c := DecisionCard(d)
		current = c.Key
		out = append(out, c)
	}
	for _, m := range existing {
		if m.CardKey == nil || *m.CardKey == current {
			continue
		}
		var data map[string]any
		if json.Unmarshal(m.Card, &data) != nil || data["state"] != decisionPending {
			continue
		}
		data["state"] = decisionResolved
		data["actions"] = []string{}
		out = append(out, Card{Key: *m.CardKey, Type: CardDecision, Body: "Decided: " + strings.TrimPrefix(m.Body, "Waiting: "), Data: data})
	}
	return out
}

// decisionKey names one wait: a phase, entered at one moment. Coming back to a phase after a
// rollback is another wait, with its own card.
func decisionKey(t db.Task) string {
	return fmt.Sprintf("decision:%s:%d", t.Phase, t.PhaseEnteredAt.Time.UnixMicro())
}

// DecisionCard is the card for a task waiting on a person.
func DecisionCard(d process.Decision) Card {
	id := uuidString(d.Task.ID)
	summary, actions := decisionWords(d)
	ref := Ref("task", id, map[string]any{"taskId": id})
	data := map[string]any{
		"ref": ref, "title": d.Task.Title, "state": decisionPending, "summary": summary, "actions": actions,
		"reason": string(d.Reason), "phase": d.Task.Phase,
	}
	if d.JobID.Valid {
		data["jobId"] = uuidString(d.JobID)
	}
	return Card{Key: decisionKey(d.Task), Type: CardDecision, Body: "Waiting: " + summary, Data: data}
}

// decisionWords says what the task waits for and what can be done about it from the card. The
// actions are the endpoints a task already has: approve, rollback, handoff, allow-agents, admit,
// close, and retry (a new job).
func decisionWords(d process.Decision) (string, []string) {
	phase := strings.ReplaceAll(d.Task.Phase, "_", " ")
	withAgents := func(actions ...string) []string {
		if !d.Task.AgentsAllowedAt.Valid {
			actions = append(actions, "allow_agents")
		}
		return actions
	}
	switch d.Reason {
	case process.ReasonBlocked:
		if d.Task.BlockedReason != nil && *d.Task.BlockedReason != "" {
			return "Blocked: " + *d.Task.BlockedReason, []string{"rollback", "handoff"}
		}
		return "Blocked in " + phase + ".", []string{"rollback", "handoff"}
	case process.ReasonExternal:
		return "Raised outside the workspace. Nothing runs on it until you let it in.", []string{"admit", "close"}
	case process.ReasonRequirementsChanged:
		return "The requirements changed after work in " + phase + " started.", []string{"approve", "rollback"}
	case process.ReasonNoRunner:
		return "No runner can take the job in " + phase + ".", []string{"handoff"}
	case process.ReasonJobFailed:
		return "The agent's job in " + phase + " failed.", withAgents("retry", "rollback", "handoff")
	case process.ReasonIdea:
		return "A new idea. Take it on, or drop it.", []string{"approve", "close"}
	}
	return capitalize(phase) + " waits for your approval.", withAgents("approve", "rollback", "handoff")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
