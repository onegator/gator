package agents

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

func id(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }

// fakeLimits stands in for the store and the process: what a chain spent, what a turn may cost,
// the project's daily budget and who may write.
type fakeLimits struct {
	chainSpent  float64
	turnCap     float64
	budget      float64
	spentToday  float64
	cannotWrite bool
}

func (f fakeLimits) ChainCost(context.Context, pgtype.UUID) (float64, error) {
	return f.chainSpent, nil
}

func (f fakeLimits) TurnPolicy(context.Context, pgtype.UUID, string, string, process.RolePolicy, string) (process.RolePolicy, error) {
	return process.RolePolicy{MaxCostUSD: f.turnCap}, nil
}

func (f fakeLimits) Budget(context.Context, pgtype.UUID, time.Time) (float64, float64, error) {
	return f.budget, f.spentToday, nil
}

func (f fakeLimits) UserCanWrite(context.Context, db.Conversation, pgtype.UUID) (bool, error) {
	return !f.cannotWrite, nil
}

func guardsOver(f fakeLimits) Guards {
	return Guards{Paused, Writers{Access: f}, HopLimit{Max: MaxHops}, NoRepeat,
		ChainBudget{Spent: f, Policies: f, Factor: DefaultChainFactor}, DailyBudget{Budgets: f}}
}

var (
	planner  = db.Agent{ID: id(1), Handle: "planner", Status: StatusActive, Tier: TierChat}
	worker   = db.Agent{ID: id(2), Handle: "worker", Status: StatusActive, Tier: TierWork}
	reviewer = db.Agent{ID: id(3), Handle: "reviewer", Status: StatusActive, Tier: TierChat}
)

// asked is a person asking an agent.
func asked(a db.Agent) Request {
	return Request{Agent: a, RequestedByKind: RequestedByUser, RequestedBy: id(9), Project: id(8)}
}

// delegated is an agent asking another, from a turn at this hop of its chain.
func delegated(from db.Agent, hop int, to db.Agent) Request {
	parent := db.AgentTurn{ID: id(byte(20 + hop)), AgentID: from.ID, ChainID: id(7), Hop: int16(hop)}
	return Request{Agent: to, RequestedByKind: RequestedByAgent, RequestedBy: from.ID, Parent: &parent, Project: id(8)}
}

// Every limit on who may be asked, one case each: the first refusal stands.
func TestTheLimitsOnATurn(t *testing.T) {
	paused := reviewer
	paused.Status = StatusPaused
	cases := []struct {
		name   string
		limits fakeLimits
		req    Request
		want   string // refusal code, "" for a turn that goes ahead
	}{
		{"a person asks an active agent", fakeLimits{}, asked(reviewer), ""},
		{"a paused agent does not answer", fakeLimits{}, asked(paused), RefusedPaused},
		{"a paused agent does not answer a delegation either", fakeLimits{}, delegated(planner, 0, paused), RefusedPaused},
		{"a person who may not write here asks nobody", fakeLimits{cannotWrite: true}, asked(reviewer), RefusedNoAccess},
		{"an agent's delegation is not a person's write", fakeLimits{cannotWrite: true}, delegated(planner, 0, worker), ""},
		{"hop 1: @planner asks @worker", fakeLimits{}, delegated(planner, 0, worker), ""},
		{"hop 3: the last turn a chain may have", fakeLimits{}, delegated(worker, 2, reviewer), ""},
		{"hop 4: the chain stops", fakeLimits{}, delegated(reviewer, 3, planner), RefusedHopLimit},
		{"hop 5: still stopped", fakeLimits{}, delegated(reviewer, 4, planner), RefusedHopLimit},
		{"the same agent twice in a row", fakeLimits{}, delegated(worker, 1, worker), RefusedRepeat},
		{"the same agent again, with another between", fakeLimits{}, delegated(reviewer, 1, worker), ""},
		{"a person may ask the agent that just answered", fakeLimits{}, asked(worker), ""},
		{"a chain under its budget", fakeLimits{chainSpent: 1.99, turnCap: 1}, delegated(planner, 0, worker), ""},
		{"a chain that spent twice a turn of the asked agent", fakeLimits{chainSpent: 2, turnCap: 1}, delegated(planner, 0, worker), RefusedChainBudget},
		{"no cap on the agent, no cap on the chain", fakeLimits{chainSpent: 100}, delegated(planner, 0, worker), ""},
		{"a person starts a new chain whatever the last one spent", fakeLimits{chainSpent: 100, turnCap: 1}, asked(worker), ""},
		{"the project spent its day", fakeLimits{budget: 10, spentToday: 10}, asked(reviewer), RefusedBudget},
		{"the project has a day left", fakeLimits{budget: 10, spentToday: 9.5}, asked(reviewer), ""},
		{"a delegation on a spent day", fakeLimits{budget: 10, spentToday: 12}, delegated(planner, 0, worker), RefusedBudget},
		{"paused comes before the hop limit", fakeLimits{}, delegated(reviewer, 3, paused), RefusedPaused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			no, err := guardsOver(tc.limits).Check(context.Background(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if no != nil {
				got = no.Code
				if no.Say == "" {
					t.Fatal("a refusal says why")
				}
			}
			if got != tc.want {
				t.Fatalf("refusal %q, want %q", got, tc.want)
			}
		})
	}
}

// A chat agent chats; a work agent works only where a person allowed agents on the task, and
// otherwise answers in chat and asks — whoever called it, a person or a chat agent's turn.
func TestWhatATurnMayTouch(t *testing.T) {
	allowed := db.Task{AgentsAllowedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
	cases := []struct {
		name  string
		agent db.Agent
		task  db.Task
		want  Grant
	}{
		{"a chat agent, no consent", reviewer, db.Task{}, Grant{Tier: TierChat}},
		{"a chat agent on an allowed task still chats", reviewer, allowed, Grant{Tier: TierChat}},
		{"a work agent without consent asks for it", worker, db.Task{}, Grant{Tier: TierChat, NeedsConsent: true}},
		{"a work agent on an allowed task works", worker, allowed, Grant{Tier: TierWork}},
	}
	for _, tc := range cases {
		if got := TierFor(tc.agent, tc.task); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
