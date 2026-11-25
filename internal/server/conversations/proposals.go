package conversations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// CardTaskProposal is a task an agent proposes, for a person to make or leave.
const CardTaskProposal = "task_proposal"

// Proposal states.
const (
	ProposalOpen     = "proposed"
	ProposalAccepted = "accepted"
)

// Proposal is a task an agent proposes in a conversation.
type Proposal struct {
	Kind, Title, Description string
	// Agent proposes it; Origin is the job it proposed it from, which keeps a repeated proposal
	// one card.
	Agent  db.Agent
	Origin pgtype.UUID
}

// Proposals puts the tasks agents propose in front of people, as cards: an agent never makes a
// task by itself, a person accepts it (with the same door and the same rules as any other).
type Proposals struct {
	pool   *pgxpool.Pool
	access *Access
	roles  RoleResolver
	tasks  TaskCreator
}

// NewProposals builds it.
func NewProposals(pool *pgxpool.Pool, access *Access, roles RoleResolver, tasks TaskCreator) *Proposals {
	return &Proposals{pool: pool, access: access, roles: roles, tasks: tasks}
}

// Propose posts the proposal as a card in conv. The same proposal from the same job is the same
// card.
func (p *Proposals) Propose(ctx context.Context, conv db.Conversation, in Proposal) (db.Message, error) {
	in.Kind, in.Title, in.Description = strings.TrimSpace(in.Kind), strings.TrimSpace(in.Title), strings.TrimSpace(in.Description)
	switch {
	case !slices.Contains(PromotableKinds, in.Kind):
		return db.Message{}, invalid("a proposed task is a %s", strings.Join(PromotableKinds, ", "))
	case in.Title == "":
		return db.Message{}, invalid("a proposed task needs a title")
	}
	sum := sha256.Sum256([]byte(in.Kind + "\x00" + in.Title))
	key := fmt.Sprintf("%s:%s:%s", CardTaskProposal, uuidString(in.Origin), hex.EncodeToString(sum[:6]))
	card := Card{Key: key, Type: CardTaskProposal,
		Body: fmt.Sprintf("@%s proposes a %s: **%s**", in.Agent.Handle, in.Kind, in.Title),
		Data: map[string]any{
			"ref":         Ref(CardTaskProposal, key, nil),
			"title":       in.Title,
			"state":       ProposalOpen,
			"summary":     in.Kind,
			"kind":        in.Kind,
			"description": in.Description,
			"agent":       agentRef(in.Agent),
			"actions":     []string{"accept"},
		}}
	var msg db.Message
	err := store.InTx(ctx, p.pool, func(q *db.Queries) error {
		var err error
		msg, _, err = writeCard(ctx, q, conv, card)
		return err
	})
	return msg, err
}

// Accept makes the proposed task, as actor: a person who may write where it was proposed and in
// its project. The card then says it was accepted and which task it became; accepting it again
// is a conflict.
func (p *Proposals) Accept(ctx context.Context, caller auth.Principal, actor process.Actor, message pgtype.UUID) (db.Task, error) {
	q := db.New(p.pool)
	msg, err := q.GetMessage(ctx, message)
	if err != nil {
		return db.Task{}, found(err)
	}
	conv, rights, err := p.access.RightsByID(ctx, msg.ConversationID, caller)
	if err != nil {
		return db.Task{}, err
	}
	if !rights.Read {
		return db.Task{}, ErrNotFound
	}
	if msg.CardType == nil || *msg.CardType != CardTaskProposal || msg.DeletedAt.Valid {
		return db.Task{}, invalid("this message proposes no task")
	}
	if !rights.Write {
		return db.Task{}, ErrForbidden
	}
	root, err := p.access.root(ctx, conv)
	if err != nil {
		return db.Task{}, err
	}
	if role, err := p.roles.ProjectRole(ctx, caller, root.ProjectID); err != nil {
		return db.Task{}, err
	} else if role < auth.RoleMember {
		return db.Task{}, ErrForbidden
	}
	var data map[string]any
	if err := json.Unmarshal(msg.Card, &data); err != nil {
		return db.Task{}, err
	}
	if data["state"] != ProposalOpen {
		return db.Task{}, conflict("this proposal was accepted already")
	}
	str := func(k string) string { s, _ := data[k].(string); return s }
	description := strings.TrimSpace(str("description") + "\n\nProposed by an agent in the conversation: " + MessageLink(msg))
	var task db.Task
	task, err = p.tasks.Create(ctx, process.CreateParams{ProjectID: root.ProjectID, Kind: str("kind"), Title: str("title"),
		Description: description, InTx: func(q *db.Queries, task db.Task) error {
			locked, err := q.GetMessageForUpdate(ctx, msg.ID)
			if err != nil {
				return err
			}
			var now map[string]any
			_ = json.Unmarshal(locked.Card, &now)
			if now["state"] != ProposalOpen {
				return conflict("this proposal was accepted a moment ago")
			}
			data["state"], data["actions"] = ProposalAccepted, []string{"open"}
			data["ref"] = Ref("task", uuidString(task.ID), map[string]any{"taskId": uuidString(task.ID)})
			_, _, err = writeCard(ctx, q, conv, Card{Key: *msg.CardKey, Type: CardTaskProposal, Body: msg.Body + " · accepted", Data: data})
			return err
		}}, actor)
	return task, err
}
