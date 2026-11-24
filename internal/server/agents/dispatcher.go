package agents

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

// dispatchStore is what the dispatcher reads. *db.Queries is one.
type dispatchStore interface {
	GetMessage(ctx context.Context, id pgtype.UUID) (db.Message, error)
	GetConversation(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
	ListMentionsOfMessages(ctx context.Context, ids []pgtype.UUID) ([]db.MessageMention, error)
	ListConversationAgentIDs(ctx context.Context, conversationID pgtype.UUID) ([]pgtype.UUID, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	GetTurn(ctx context.Context, id pgtype.UUID) (db.AgentTurn, error)
}

// turnStarter starts or refuses a turn. Starter is one.
type turnStarter interface {
	Start(ctx context.Context, r Request) error
	Refuse(ctx context.Context, r Request, no Refusal) error
}

// Interceptor sees a message before it asks anyone, and may take it as something else for some
// of the agents it asks — a person steering an agent that is answering there. It answers the
// agents the message still asks for a turn.
type Interceptor interface {
	Intercept(ctx context.Context, msg db.Message, conv db.Conversation, asked []db.Agent) ([]db.Agent, error)
}

// Dispatcher answers what is said to agents: every message.posted that mentions an agent, or
// that a person writes in a DM with one, becomes a turn of each agent asked, or a refusal that
// says why. Who may be asked is the guards' business, and what follows is the starter's.
type Dispatcher struct {
	store        dispatchStore
	guards       Guard
	turns        turnStarter
	interceptors []Interceptor
}

// NewDispatcher builds it. Interceptors are asked in order before any turn starts.
func NewDispatcher(store dispatchStore, guards Guard, turns turnStarter, interceptors ...Interceptor) *Dispatcher {
	return &Dispatcher{store: store, guards: guards, turns: turns, interceptors: interceptors}
}

// Events are the outbox events the dispatcher reads.
func (*Dispatcher) Events() []string { return []string{conversations.EventMessagePosted} }

// Handle implements conversations.Handler.
func (d *Dispatcher) Handle(ctx context.Context, e db.EventsAfterRow) error {
	var p struct {
		MessageID string `json:"messageId"`
	}
	var id pgtype.UUID
	if json.Unmarshal(e.Payload, &p) != nil || id.Scan(p.MessageID) != nil {
		return nil
	}
	msg, err := d.store.GetMessage(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !asks(msg) {
		return nil
	}
	conv, err := d.store.GetConversation(ctx, msg.ConversationID)
	if err != nil {
		return ignoreGone(err)
	}
	if !conv.ProjectID.Valid {
		return nil // agents are a project's
	}
	asked, err := d.asked(ctx, msg, conv)
	if err != nil {
		return err
	}
	for _, in := range d.interceptors {
		if asked, err = in.Intercept(ctx, msg, conv, asked); err != nil {
			return err
		}
	}
	parent, err := d.parent(ctx, msg)
	if err != nil {
		return err
	}
	for _, agent := range asked {
		r := Request{Agent: agent, Trigger: msg, Conversation: conv, Project: conv.ProjectID,
			RequestedByKind: msg.AuthorKind, RequestedBy: msg.AuthorID, Parent: parent}
		no, err := d.guards.Check(ctx, r)
		if err != nil {
			return err
		}
		if no != nil {
			err = d.turns.Refuse(ctx, r, *no)
		} else {
			err = d.turns.Start(ctx, r)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// asks says whether a message can ask an agent anything: words a person or an agent wrote, still
// there. Cards, Gator's own lines, plugins and the outside world do not call agents.
func asks(m db.Message) bool {
	return !m.DeletedAt.Valid && m.Kind == "text" && m.Status == conversations.StatusFinal &&
		(m.AuthorKind == conversations.AuthorUser || m.AuthorKind == conversations.AuthorAgent)
}

// asked are the agents a message asks: those it mentions and, for a person's message in a DM,
// the agents in that DM. An agent's own words ask only whom they mention.
func (d *Dispatcher) asked(ctx context.Context, msg db.Message, conv db.Conversation) ([]db.Agent, error) {
	var ids []pgtype.UUID
	mentions, err := d.store.ListMentionsOfMessages(ctx, []pgtype.UUID{msg.ID})
	if err != nil {
		return nil, err
	}
	for _, m := range mentions {
		if m.MemberKind == conversations.MemberAgent {
			ids = append(ids, m.MemberID)
		}
	}
	if conv.Kind == conversations.KindDM && msg.AuthorKind == conversations.AuthorUser {
		members, err := d.store.ListConversationAgentIDs(ctx, conv.ID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, members...)
	}
	seen := map[[16]byte]bool{}
	var out []db.Agent
	for _, id := range ids {
		if seen[id.Bytes] {
			continue
		}
		seen[id.Bytes] = true
		a, err := d.store.GetAgent(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && a.DeletedAt.Valid) {
			continue // put away since
		}
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// parent is the turn an agent's message was said in: what it starts belongs to that chain.
func (d *Dispatcher) parent(ctx context.Context, msg db.Message) (*db.AgentTurn, error) {
	if msg.AuthorKind != conversations.AuthorAgent {
		return nil, nil
	}
	var id pgtype.UUID
	if id.Scan(conversations.OriginOf(msg).Turn) != nil || !id.Valid {
		return nil, nil
	}
	t, err := d.store.GetTurn(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func ignoreGone(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
