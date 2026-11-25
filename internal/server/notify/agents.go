package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Categories of actionable notifications: the app registers them with their buttons (Allow and
// Deny for a tool approval; Deny opens the app to say why).
const (
	CategoryToolApproval = "TOOL_APPROVAL"
	CategoryQuestion     = "QUESTION"
	CategoryTurnFinished = "TURN_FINISHED"
)

// Rule is what the notifier does with one kind of event besides the rules it was built with. A
// new kind of notification is one more rule (On), not a change to the notifier.
type Rule func(ctx context.Context, n *Notifier, row db.EventsAfterRow) error

// On has the notifier apply rule to every event of these types, after its own rules.
func (n *Notifier) On(rule Rule, types ...string) *Notifier {
	if n.rules == nil {
		n.rules = map[string][]Rule{}
	}
	for _, t := range types {
		n.rules[t] = append(n.rules[t], rule)
	}
	return n
}

// WithAgents has the notifier tell people about their agents' turns: a tool approval or a question
// waiting for them (the person the turn acts for, else the task's owner), and an answer they asked
// for, written. Only somebody who may read where the agent works hears of it.
func (n *Notifier) WithAgents() *Notifier {
	return n.On(waitingRule, "runtime_request.created").On(finishedRule, "job.finished")
}

// waitingRule pushes a request an agent waits on to the person it waits for.
func waitingRule(ctx context.Context, n *Notifier, row db.EventsAfterRow) error {
	var p struct {
		RequestID string `json:"request_id"`
	}
	var id pgtype.UUID
	if json.Unmarshal(row.Payload, &p) != nil || id.Scan(p.RequestID) != nil {
		return nil
	}
	q := db.New(n.pool)
	req, err := q.GetRuntimeRequest(ctx, id)
	if err != nil {
		return ignoreGone(err)
	}
	if req.Status != "pending" {
		return nil // decided before anybody needed telling
	}
	job, err := q.GetJob(ctx, req.JobID)
	if err != nil {
		return ignoreGone(err)
	}
	task, err := q.GetTask(ctx, job.TaskID)
	if err != nil {
		return ignoreGone(err)
	}
	var turn *db.AgentTurn
	if job.TurnID.Valid {
		if t, err := q.GetTurn(ctx, job.TurnID); err == nil {
			turn = &t
		}
	}
	who := whoWaits(turn, task)
	if !who.Valid {
		return nil
	}
	ok, err := n.mayRead(ctx, q, job, turn, who)
	if err != nil || !ok {
		return err
	}
	agent := "An agent"
	if job.AgentID.Valid {
		if a, err := q.GetAgent(ctx, job.AgentID); err == nil {
			agent = "@" + a.Handle
		}
	}
	return n.deliver(ctx, []pgtype.UUID{who}, waitingMessage(req, agent, task))
}

// whoWaits is the person a request waits for: the one the turn acts for, else the task's owner.
func whoWaits(turn *db.AgentTurn, task db.Task) pgtype.UUID {
	if turn != nil && turn.OnBehalfOf.Valid {
		return turn.OnBehalfOf
	}
	if task.OwnerKind != nil && *task.OwnerKind == "user" && task.OwnerID.Valid {
		return task.OwnerID
	}
	return pgtype.UUID{}
}

// waitingMessage is the notification of a request: what the agent asks, with its buttons.
func waitingMessage(req db.RuntimeRequest, agent string, task db.Task) Message {
	m := Message{Title: fmt.Sprintf("%s asks to use %s", agent, deref(req.Tool)), Link: "gator://runtime-requests/" + uuidString(req.ID),
		Category: CategoryToolApproval, Data: map[string]string{"request_id": uuidString(req.ID), "task_id": uuidString(task.ID)}}
	if req.Kind == "question" {
		m.Title, m.Category = agent+" asks you a question", CategoryQuestion
	}
	m.Body = clipped(requestLine(req), 160) + " · " + task.Title
	return m
}

// requestLine is one line of what a request is about: the command, the URL, or the rule.
func requestLine(req db.RuntimeRequest) string {
	var in map[string]any
	_ = json.Unmarshal(req.Input, &in)
	for _, k := range []string{"command", "url", "file_path", "question"} {
		if s, ok := in[k].(string); ok && s != "" {
			return strings.Join(strings.Fields(s), " ")
		}
	}
	if req.Rule != "" {
		return req.Rule
	}
	return "a request"
}

// finishedRule tells the person who asked an agent that its answer is written.
func finishedRule(ctx context.Context, n *Notifier, row db.EventsAfterRow) error {
	q := db.New(n.pool)
	job, err := q.GetJob(ctx, row.AggregateID)
	if err != nil {
		return ignoreGone(err)
	}
	if job.Kind != "reply" || job.Status != "done" || !job.TurnID.Valid {
		return nil // a failure is told by the task's own rule; a phase's work by its gate
	}
	turn, err := q.GetTurn(ctx, job.TurnID)
	if err != nil {
		return ignoreGone(err)
	}
	if turn.RequestedByKind != "user" || !turn.RequestedBy.Valid {
		return nil
	}
	ok, err := n.mayRead(ctx, q, job, &turn, turn.RequestedBy)
	if err != nil || !ok {
		return err
	}
	agent := "The agent"
	if a, err := q.GetAgent(ctx, turn.AgentID); err == nil {
		agent = "@" + a.Handle
	}
	link := "gator://conversations/" + uuidString(turn.ConversationID)
	return n.deliver(ctx, []pgtype.UUID{turn.RequestedBy}, Message{Title: agent + " answered", Body: "Your question has its answer.",
		Link: link, Category: CategoryTurnFinished, Data: map[string]string{"turn_id": uuidString(turn.ID)}})
}

// mayRead says whether a person may read where the job's agent works: the turn's conversation, or
// for a job with none, the task's project (a member).
func (n *Notifier) mayRead(ctx context.Context, q *db.Queries, job db.Job, turn *db.AgentTurn, user pgtype.UUID) (bool, error) {
	if turn != nil && n.chat != nil {
		conv, err := q.GetConversation(ctx, turn.ConversationID)
		if err != nil {
			return false, ignoreGone(err)
		}
		return n.chat.UserCanRead(ctx, conv, user)
	}
	members, err := q.ListProjectMemberIDs(ctx, job.ProjectID)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if m == user {
			return true, nil
		}
	}
	return false, nil
}

func ignoreGone(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func clipped(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
