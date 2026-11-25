package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

// Card types.
const (
	CardToolApproval = "tool_approval"
	CardQuestion     = "question"
)

// cardStore is what the cards read. *db.Queries is one.
type cardStore interface {
	GetRuntimeRequest(ctx context.Context, id pgtype.UUID) (db.RuntimeRequest, error)
	GetJob(ctx context.Context, id pgtype.UUID) (db.Job, error)
	GetTask(ctx context.Context, id pgtype.UUID) (db.Task, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	GetUser(ctx context.Context, id pgtype.UUID) (db.User, error)
}

// Cards shows each request in the thread of its job's task, as one card that follows it from
// waiting to decided: a conversations.CardSource.
type Cards struct {
	store cardStore
}

// NewCards builds it.
func NewCards(store cardStore) Cards { return Cards{store: store} }

// Events implements conversations.CardSource.
func (Cards) Events() []string {
	return []string{EventCreated, EventDecided, EventExpired, EventCancelled}
}

// Cards implements conversations.CardSource.
func (c Cards) Cards(ctx context.Context, e db.EventsAfterRow) ([]conversations.PlacedCard, error) {
	var p struct {
		RequestID string `json:"request_id"`
	}
	var id pgtype.UUID
	if json.Unmarshal(e.Payload, &p) != nil || id.Scan(p.RequestID) != nil {
		return nil, nil
	}
	req, err := c.store.GetRuntimeRequest(ctx, id)
	if err != nil {
		return nil, ignoreGone(err)
	}
	job, err := c.store.GetJob(ctx, req.JobID)
	if err != nil {
		return nil, ignoreGone(err)
	}
	task, err := c.store.GetTask(ctx, job.TaskID)
	if err != nil {
		return nil, ignoreGone(err)
	}
	var agent *db.Agent
	if job.AgentID.Valid {
		a, err := c.store.GetAgent(ctx, job.AgentID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			agent = &a
		}
	}
	by := ""
	if req.DecidedBy.Valid {
		if u, err := c.store.GetUser(ctx, req.DecidedBy); err == nil {
			by = u.Name
		}
	}
	return []conversations.PlacedCard{{Task: task, Card: Card(req, job, agent, by)}}, nil
}

// maxCardInput bounds the tool's input a card shows.
const maxCardInput = 2000

// Card is the card of one request: which agent asks to run which tool with what (redacted), and
// what a person can do — allow, allow until the session ends, deny with a reason, stop the turn;
// for a question, answer it or stop. A decided card says who decided what, and has no buttons.
func Card(r db.RuntimeRequest, job db.Job, agent *db.Agent, by string) conversations.Card {
	who := "The agent"
	if agent != nil {
		who = "@" + agent.Handle
	}
	tool := deref(r.Tool)
	typ, title := CardToolApproval, fmt.Sprintf("%s asks to use %s", who, tool)
	actions := []string{DecideAllow, DecideAllowForSession, DecideDeny, DecideStop}
	summary := inputSummary(r)
	if r.Kind == proto.RequestQuestion {
		typ, title = CardQuestion, fmt.Sprintf("%s asks you a question", who)
		actions = []string{"answer", DecideStop}
	}
	state := r.Status
	if r.Status != StatusPending {
		actions = []string{}
	}
	body := title + ": " + summary
	if note := decidedNote(r, by); note != "" {
		body += " · " + note
	}
	id := uuidString(r.ID)
	data := map[string]any{
		"ref":       conversations.Ref("runtime_request", id, map[string]any{"taskId": uuidString(job.TaskID), "jobId": uuidString(job.ID)}),
		"title":     title,
		"state":     state,
		"summary":   summary,
		"actions":   actions,
		"tool":      tool,
		"rule":      r.Rule,
		"input":     clipInput(r.Input),
		"expiresAt": r.ExpiresAt.Time,
	}
	if agent != nil {
		data["agent"] = conversations.AgentRef(*agent)
	}
	if by != "" {
		data["decidedBy"] = by
	}
	if r.Feedback != nil && r.Status != StatusPending {
		data["feedback"] = *r.Feedback
	}
	return conversations.Card{Key: "approval:" + id, Type: typ, Body: body, Data: data}
}

// inputSummary is the one line a card says of what the tool would do: the command, the URL, the
// file — or the question.
func inputSummary(r db.RuntimeRequest) string {
	var in map[string]any
	_ = json.Unmarshal(r.Input, &in)
	if r.Kind == proto.RequestQuestion {
		if qs, ok := in["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				if s, ok := q["question"].(string); ok {
					return oneLine(s)
				}
			}
		}
		if s, ok := in["question"].(string); ok {
			return oneLine(s)
		}
		return "a question"
	}
	for _, k := range []string{"command", "url", "file_path", "path", "query"} {
		if s, ok := in[k].(string); ok && s != "" {
			return oneLine(s)
		}
	}
	if r.Rule != "" {
		return r.Rule
	}
	return deref(r.Tool)
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// clipInput is the input a card carries, as an object, bounded.
func clipInput(raw []byte) any {
	if len(raw) > maxCardInput {
		return map[string]any{"note": fmt.Sprintf("%d bytes; see the request", len(raw))}
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return map[string]any{}
	}
	return v
}

func decidedNote(r db.RuntimeRequest, by string) string {
	who := orText(by, "a person")
	switch {
	case r.Status == StatusAllowed && r.Kind == proto.RequestQuestion:
		return "answered by " + who
	case r.Status == StatusAllowed:
		return "allowed by " + who
	case r.Status == StatusAllowedForSession:
		return "allowed for the session by " + who
	case r.Status == StatusDenied && r.Stopped:
		return "the turn was stopped by " + who
	case r.Status == StatusDenied:
		return "denied by " + who
	case r.Status == StatusExpired:
		return "nobody answered in time: denied"
	case r.Status == StatusCancelled:
		return "the turn ended"
	}
	return ""
}

// Handler cancels what a job that ended was waiting for: a conversations.Handler on job.finished.
func (s *Service) Handler() conversations.Handler {
	return conversations.HandlerFunc(func(ctx context.Context, e db.EventsAfterRow) error {
		return s.CancelForJob(ctx, e.AggregateID)
	})
}
