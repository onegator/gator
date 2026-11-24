package agents

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// CardConsent asks a person to let a work agent that answered in chat work on the task.
const CardConsent = "consent"

// Consent states.
const (
	ConsentPending = "pending"
	ConsentAllowed = "allowed"
)

// cardSayer posts and updates Gator's cards. conversations.Voice is one.
type cardSayer interface {
	Card(ctx context.Context, conv db.Conversation, c conversations.Card) (db.Message, error)
}

// agentsAllower records a person's consent to agents on a task. process.Service is one.
type agentsAllower interface {
	AllowAgents(ctx context.Context, taskID pgtype.UUID, actor process.Actor) (db.Task, error)
}

// roleResolver answers a caller's role in a project. auth.Authorizer is one.
type roleResolver interface {
	ProjectRole(ctx context.Context, p auth.Principal, project pgtype.UUID) (auth.Role, error)
}

// consentStore is what Consent reads. *db.Queries is one.
type consentStore interface {
	GetTurn(ctx context.Context, id pgtype.UUID) (db.AgentTurn, error)
	GetJob(ctx context.Context, id pgtype.UUID) (db.Job, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	GetMessage(ctx context.Context, id pgtype.UUID) (db.Message, error)
	GetConversation(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
	FindCard(ctx context.Context, arg db.FindCardParams) (db.Message, error)
	GetTurnByTrigger(ctx context.Context, arg db.GetTurnByTriggerParams) (db.AgentTurn, error)
}

// Consent is the card "Let @worker work on this?": a work agent asked where nobody allowed
// agents answers in chat, and this card asks. Allowing it is the task's consent (allow-agents,
// the rule of migration 00033, given by the person who clicks) and a new turn of the agent, in
// the work tier, answering the same question. Nothing here goes around that rule.
type Consent struct {
	store   consentStore
	cards   cardSayer
	rights  conversationRights
	roles   roleResolver
	allower agentsAllower
	guards  Guard
	turns   turnStarter
}

// NewConsent builds it.
func NewConsent(store consentStore, cards cardSayer, rights conversationRights, roles roleResolver, allower agentsAllower, guards Guard, turns turnStarter) *Consent {
	return &Consent{store: store, cards: cards, rights: rights, roles: roles, allower: allower, guards: guards, turns: turns}
}

func consentKey(turn pgtype.UUID) string { return "consent:" + uuidText(turn) }

// consentCard is the card of a turn's question, in one of its states.
func consentCard(turn db.AgentTurn, agent db.Agent, task db.Task, state string, by string) conversations.Card {
	body := fmt.Sprintf("Let @%s work on this? It answered in chat, without tools that write; working, it changes code on a gator/ branch.", agent.Handle)
	actions := []string{"allow"}
	if state == ConsentAllowed {
		body = fmt.Sprintf("@%s may work on this: %s allowed agents on the task.", agent.Handle, by)
		actions = []string{}
	}
	return conversations.Card{Key: consentKey(turn.ID), Type: CardConsent, Body: body, Data: map[string]any{
		"ref":     conversations.Ref("turn", uuidText(turn.ID), map[string]any{"taskId": uuidText(task.ID)}),
		"title":   fmt.Sprintf("Let @%s work on this?", agent.Handle),
		"state":   state,
		"summary": task.Title,
		"agent":   conversations.AgentRef(agent),
		"actions": actions,
	}}
}

// consentCardStore is what ConsentCards reads. *db.Queries is one.
type consentCardStore interface {
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	FindCard(ctx context.Context, arg db.FindCardParams) (db.Message, error)
}

// ConsentCards posts the question (ConsentAsker); Consent answers it.
type ConsentCards struct {
	store consentCardStore
	cards cardSayer
}

// NewConsentCards builds it.
func NewConsentCards(store consentCardStore, cards cardSayer) *ConsentCards {
	return &ConsentCards{store: store, cards: cards}
}

// AskConsent implements ConsentAsker: the card goes where the agent answered, once; a card
// already there, asked or answered, is left as it is.
func (c *ConsentCards) AskConsent(ctx context.Context, turn db.AgentTurn, home db.Conversation, task db.Task) error {
	_, err := c.store.FindCard(ctx, db.FindCardParams{CardKey: new(consentKey(turn.ID)), ConversationIds: []pgtype.UUID{home.ID}})
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	agent, err := c.store.GetAgent(ctx, turn.AgentID)
	if err != nil {
		return err
	}
	_, err = c.cards.Card(ctx, home, consentCard(turn, agent, task, ConsentPending, ""))
	return err
}

// Allow is a person letting the agent of a chat turn work: agents are allowed on its task, in
// that person's name, and the agent takes a work turn on the same question. It needs a member of
// the project who may write in the conversation; doing it again answers the turn it started.
func (c *Consent) Allow(ctx context.Context, p auth.Principal, turnID pgtype.UUID) (db.AgentTurn, error) {
	turn, err := c.store.GetTurn(ctx, turnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTurn{}, ErrNotFound
	}
	if err != nil {
		return db.AgentTurn{}, err
	}
	conv, rights, err := c.rights.RightsByID(ctx, turn.ConversationID, p)
	if err != nil {
		return db.AgentTurn{}, err
	}
	if !rights.Read {
		return db.AgentTurn{}, ErrNotFound
	}
	card, err := c.store.FindCard(ctx, db.FindCardParams{CardKey: new(consentKey(turn.ID)), ConversationIds: []pgtype.UUID{conv.ID}})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTurn{}, fmt.Errorf("%w: nobody was asked to let this agent work", ErrInvalid)
	}
	if err != nil {
		return db.AgentTurn{}, err
	}
	if role, err := c.roles.ProjectRole(ctx, p, turn.ProjectID); err != nil {
		return db.AgentTurn{}, err
	} else if !rights.Write || role < auth.RoleMember || p.Kind != auth.KindUser {
		return db.AgentTurn{}, ErrForbidden
	}
	agent, err := c.store.GetAgent(ctx, turn.AgentID)
	if err != nil {
		return db.AgentTurn{}, err
	}
	if !turn.JobID.Valid {
		return db.AgentTurn{}, fmt.Errorf("%w: this turn never started", ErrConflict)
	}
	job, err := c.store.GetJob(ctx, turn.JobID)
	if err != nil {
		return db.AgentTurn{}, err
	}
	task, err := c.allower.AllowAgents(ctx, job.TaskID, process.Actor{Kind: process.ActorUser, ID: p.UserID})
	if err != nil {
		return db.AgentTurn{}, err
	}
	if _, err := c.cards.Card(ctx, conv, consentCard(turn, agent, task, ConsentAllowed, orText(p.Name, "a person"))); err != nil {
		return db.AgentTurn{}, err
	}
	ask, err := c.store.GetMessage(ctx, turn.TriggerMessageID)
	if err != nil {
		return db.AgentTurn{}, err
	}
	r := Request{Agent: agent, Trigger: card, Conversation: conv, Project: turn.ProjectID,
		RequestedByKind: RequestedByUser, RequestedBy: p.UserID, EscalatedFrom: &turn, Ask: &ask}
	no, err := c.guards.Check(ctx, r)
	if err != nil {
		return db.AgentTurn{}, err
	}
	if no != nil {
		err = c.turns.Refuse(ctx, r, *no)
	} else {
		err = c.turns.Start(ctx, r)
	}
	if err != nil {
		return db.AgentTurn{}, err
	}
	return c.store.GetTurnByTrigger(ctx, db.GetTurnByTriggerParams{AgentID: agent.ID, TriggerMessageID: card.ID})
}
