package agents

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Who asks an agent for a turn (agent_turns.requested_by_kind).
const (
	RequestedByUser       = "user"
	RequestedByAgent      = "agent"
	RequestedByAutomation = "automation"
	RequestedByProcess    = "process"
)

// Request is one call for an agent's turn: who is asked, by which message, by whom, and the turn
// that delegated it when an agent asked.
type Request struct {
	Agent db.Agent
	// Trigger is the message that asked; Conversation is where it was posted.
	Trigger      db.Message
	Conversation db.Conversation
	// Project is the project the conversation belongs to.
	Project pgtype.UUID
	// RequestedBy is the person or the agent who wrote the trigger.
	RequestedByKind string
	RequestedBy     pgtype.UUID
	// Parent is the turn an agent was in when it asked; nil when a person asked.
	Parent *db.AgentTurn
}

// Hop is how far down a chain of delegations the turn is: 0 when a person asks, one more than
// the turn that delegated otherwise.
func (r Request) Hop() int {
	if r.Parent == nil {
		return 0
	}
	return int(r.Parent.Hop) + 1
}

// Chain is the chain the turn belongs to: its parent's, or none yet (a person starts a new one).
func (r Request) Chain() (pgtype.UUID, bool) {
	if r.Parent == nil {
		return pgtype.UUID{}, false
	}
	return r.Parent.ChainID, true
}

// OnBehalfOf is the person at the start of the chain.
func (r Request) OnBehalfOf() pgtype.UUID {
	if r.Parent != nil {
		return r.Parent.OnBehalfOf
	}
	if r.RequestedByKind == RequestedByUser {
		return r.RequestedBy
	}
	return pgtype.UUID{}
}

// Refusal is why an agent does not take a turn, in a code for the record (agent_turns.refusal)
// and a line for the conversation.
type Refusal struct {
	Code string
	Say  string
}

// Refusal codes.
const (
	RefusedPaused      = "paused"
	RefusedNoAccess    = "no_access"
	RefusedHopLimit    = "hop_limit"
	RefusedRepeat      = "repeat"
	RefusedChainBudget = "chain_budget"
	RefusedBudget      = "budget"
	RefusedNoTask      = "no_task"
)
