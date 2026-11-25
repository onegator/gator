package agents

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Who asks an agent for a turn (agent_turns.requested_by_kind also allows automation and
// process, for the phases to come).
const (
	RequestedByUser  = "user"
	RequestedByAgent = "agent"
	// RequestedByExternal is somebody outside the workspace: on a bridge (Slack, a comment on a
	// pull request) or an A2A client. They are heard, in chat; they never let an agent work.
	RequestedByExternal = "external"
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
	// FromJob is the job an agent asked from when that job answers no turn (a phase job): the
	// chain starts at that job, which is its first turn.
	FromJob pgtype.UUID
	// EscalatedFrom is the chat turn a person let work: this turn answers that turn's ask, and
	// its trigger is the person's consent.
	EscalatedFrom *db.AgentTurn
	// Ask is the message the turn answers when its trigger is not it (an escalation's).
	Ask *db.Message
}

// Question is the message the turn answers.
func (r Request) Question() db.Message {
	if r.Ask != nil {
		return *r.Ask
	}
	return r.Trigger
}

// External says the turn was asked for from outside the workspace.
func (r Request) External() bool { return r.RequestedByKind == RequestedByExternal }

// Hop is how far down a chain of delegations the turn is: 0 when a person asks, one more than
// the turn that delegated otherwise.
func (r Request) Hop() int {
	switch {
	case r.Parent != nil:
		return int(r.Parent.Hop) + 1
	case r.FromJob.Valid:
		return 1
	}
	return 0
}

// Chain is the chain the turn belongs to: its parent's, the phase job's it was asked from, or
// none yet (a person starts a new one).
func (r Request) Chain() (pgtype.UUID, bool) {
	switch {
	case r.Parent != nil:
		return r.Parent.ChainID, true
	case r.FromJob.Valid:
		return r.FromJob, true
	}
	return pgtype.UUID{}, false
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
	RefusedFanOut      = "fan_out"
	RefusedAgentBudget = "agent_budget"
)
