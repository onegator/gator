package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// Guard is one reason an agent may refuse a turn. Each guard asks one question; the dispatcher
// asks them in order and the first refusal stands. A new limit is a new guard, not an edit here.
type Guard interface {
	// Check answers a refusal, or nil when this guard lets the turn through.
	Check(ctx context.Context, r Request) (*Refusal, error)
}

// GuardFunc is a function that is a Guard.
type GuardFunc func(ctx context.Context, r Request) (*Refusal, error)

// Check implements Guard.
func (f GuardFunc) Check(ctx context.Context, r Request) (*Refusal, error) { return f(ctx, r) }

// Guards are guards asked in order.
type Guards []Guard

// Check answers the first refusal.
func (gs Guards) Check(ctx context.Context, r Request) (*Refusal, error) {
	for _, g := range gs {
		if no, err := g.Check(ctx, r); err != nil || no != nil {
			return no, err
		}
	}
	return nil, nil
}

// Paused refuses a turn of a paused agent.
var Paused = GuardFunc(func(_ context.Context, r Request) (*Refusal, error) {
	if r.Agent.Status == StatusPaused {
		return &Refusal{Code: RefusedPaused, Say: fmt.Sprintf("@%s is paused and does not answer until a project admin resumes it.", r.Agent.Handle)}, nil
	}
	return nil, nil
})

// writeChecker answers whether a person may write in a conversation. conversations.Access is one.
type writeChecker interface {
	UserCanWrite(ctx context.Context, conv db.Conversation, user pgtype.UUID) (bool, error)
}

// Writers refuses a turn a person asked for in a conversation they may not write in (any more):
// asking an agent is saying something there. An agent asks from a turn it was given.
type Writers struct{ Access writeChecker }

// Check implements Guard.
func (g Writers) Check(ctx context.Context, r Request) (*Refusal, error) {
	if r.RequestedByKind != RequestedByUser {
		return nil, nil
	}
	ok, err := g.Access.UserCanWrite(ctx, r.Conversation, r.RequestedBy)
	if err != nil || ok {
		return nil, err
	}
	return &Refusal{Code: RefusedNoAccess, Say: fmt.Sprintf("@%s answers people who may write here.", r.Agent.Handle)}, nil
}

// MaxHops is how many turns one chain may have: a person's call and three delegations after it.
const MaxHops = 4

// HopLimit refuses a delegation that would make a chain longer than Max turns.
type HopLimit struct{ Max int }

// Check implements Guard.
func (g HopLimit) Check(_ context.Context, r Request) (*Refusal, error) {
	if r.Hop() < g.Max {
		return nil, nil
	}
	return &Refusal{Code: RefusedHopLimit, Say: fmt.Sprintf(
		"This chain of agents stopped at @%s: %d turns in a row without a person is the limit. Reply here to go on.", r.Agent.Handle, g.Max)}, nil
}

// NoRepeat refuses an agent asking itself: the same agent twice in a row in a chain, with no
// person between, is a loop rather than a delegation.
var NoRepeat = GuardFunc(func(_ context.Context, r Request) (*Refusal, error) {
	if r.Parent == nil || r.Parent.AgentID != r.Agent.ID {
		return nil, nil
	}
	return &Refusal{Code: RefusedRepeat, Say: fmt.Sprintf("@%s cannot ask itself; a person or another agent has to answer first.", r.Agent.Handle)}, nil
})

// chainSpender answers what a chain has spent. *db.Queries is one.
type chainSpender interface {
	ChainCost(ctx context.Context, chainID pgtype.UUID) (float64, error)
}

// turnPolicies resolves what a turn of an agent may cost. process.Service is one.
type turnPolicies interface {
	TurnPolicy(ctx context.Context, projectID pgtype.UUID, role, tier string, own process.RolePolicy, defaultBackend string) (process.RolePolicy, error)
}

// ChainBudget refuses a delegation once its chain has spent Factor times what one turn of the
// asked agent may cost. An agent with no cap has no chain cap either: the hop limit still holds.
type ChainBudget struct {
	Spent    chainSpender
	Policies turnPolicies
	Factor   float64
}

// DefaultChainFactor is the chain's budget in turns of the asked agent.
const DefaultChainFactor = 2

// Check implements Guard.
func (g ChainBudget) Check(ctx context.Context, r Request) (*Refusal, error) {
	chain, ok := r.Chain()
	if !ok {
		return nil, nil
	}
	pol, err := g.Policies.TurnPolicy(ctx, r.Project, roleOf(r.Agent), r.Agent.Tier, ownPolicy(r.Agent), "")
	if err != nil || pol.MaxCostUSD <= 0 {
		return nil, err
	}
	spent, err := g.Spent.ChainCost(ctx, chain)
	if err != nil {
		return nil, err
	}
	limit := g.Factor * pol.MaxCostUSD
	if spent < limit {
		return nil, nil
	}
	return &Refusal{Code: RefusedChainBudget, Say: fmt.Sprintf(
		"This chain of agents stopped before @%s: it has spent $%.2f of its $%.2f. Reply here to go on.", r.Agent.Handle, spent, limit)}, nil
}

// budgets answers a project's daily budget and what it spent today. process.Service is one.
type budgets interface {
	Budget(ctx context.Context, projectID pgtype.UUID, now time.Time) (limit, spent float64, err error)
}

// DailyBudget refuses any turn once the project spent its daily budget, as it refuses jobs.
type DailyBudget struct {
	Budgets budgets
	Now     func() time.Time
}

// Check implements Guard.
func (g DailyBudget) Check(ctx context.Context, r Request) (*Refusal, error) {
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	limit, spent, err := g.Budgets.Budget(ctx, r.Project, now())
	if err != nil || limit <= 0 || spent < limit {
		return nil, err
	}
	return &Refusal{Code: RefusedBudget, Say: fmt.Sprintf(
		"@%s does not answer today: the project spent $%.2f of its $%.2f daily budget.", r.Agent.Handle, spent, limit)}, nil
}

// roleOf is the role an agent plays, for its policy and prompt; one without a role is an agent.
func roleOf(a db.Agent) string {
	if a.RoleKey != nil && *a.RoleKey != "" {
		return *a.RoleKey
	}
	return "agent"
}

// ownPolicy is the policy an agent has of its own; one that does not read is none.
func ownPolicy(a db.Agent) process.RolePolicy {
	var p process.RolePolicy
	_ = json.Unmarshal(a.Policy, &p)
	return p
}
