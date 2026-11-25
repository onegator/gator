package process

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// RolePolicy says which backend and model run a role and what one job may cost.
type RolePolicy struct {
	Backend    string  `json:"backend,omitempty"`
	Model      string  `json:"model,omitempty"`
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
	// Approvals, in an agent's own policy, tightens the project's (Approvals.Tighten); in a role's
	// or a tier's it means nothing.
	Approvals *Approvals `json:"approvals,omitempty"`
}

// Policy is the project's choice of backends and models per role, and its daily spend limit.
// There is no automatic fallback: a job waits for a runner with its backend logged in rather
// than running somewhere the owner did not choose.
type Policy struct {
	Default RolePolicy            `json:"default"`
	Roles   map[string]RolePolicy `json:"roles"`
	// Tiers applies to an agent's turns in a conversation by what they may touch, over the
	// role's entry: {"chat": {"model": "claude-haiku-5"}} answers mentions with a cheaper model
	// than the one that does the phase. Phase jobs are not turns and ignore it.
	Tiers          map[string]RolePolicy `json:"tiers,omitempty"`
	DailyBudgetUSD float64               `json:"daily_budget_usd"`
	// Placement is where the autopilot's jobs run: "local" (a machine of the person who
	// started the task, or any shared one) or "cloud". Empty is local. A person starting a
	// job by hand chooses for themselves.
	Placement string `json:"placement,omitempty"`
	// Approvals are the tools of a work turn that wait for a person; nil is DefaultApprovals.
	Approvals *Approvals `json:"approvals,omitempty"`
}

// backendName is what names a backend: a runner's built-in one (claude) or an agent speaking
// ACP through a runner (acp:codex), as GATOR_ACP_AGENTS names it.
var backendName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}(:[a-z][a-z0-9-]{0,31})?$`)

// ValidBackend says whether s can name a backend in a policy. Which runners have it is not the
// policy's business: a job waits for a runner with its backend logged in.
func ValidBackend(s string) bool { return backendName.MatchString(s) }

// CheckBackends refuses a configuration that names a backend no runner could ever advertise (or
// approvals a runner could not hold), at the moment it is written: a typo would otherwise leave
// every job of the role waiting for good.
func (c ProjectConfig) CheckBackends() error {
	check := func(where, b string) error {
		if b != "" && !ValidBackend(b) {
			return fmt.Errorf("process_config: %s backend %q is not a backend name (claude, acp:<agent>)", where, b)
		}
		return nil
	}
	if c.Autopilot != nil {
		if err := check("autopilot", c.Autopilot.Backend); err != nil {
			return err
		}
	}
	if c.Policy == nil {
		return nil
	}
	if a := c.Policy.Approvals; a != nil {
		if err := a.Check(); err != nil {
			return err
		}
	}
	if err := check("policy.default", c.Policy.Default.Backend); err != nil {
		return err
	}
	for role, p := range c.Policy.Roles {
		if err := check("policy.roles."+role, p.Backend); err != nil {
			return err
		}
	}
	for tier, p := range c.Policy.Tiers {
		if err := check("policy.tiers."+tier, p.Backend); err != nil {
			return err
		}
	}
	return nil
}

// Placement is where the project's autopilot jobs run: "local" unless its policy says "cloud".
func (c ProjectConfig) Placement() string {
	if c.Policy != nil && c.Policy.Placement == "cloud" {
		return "cloud"
	}
	return "local"
}

// JobPlacement is the project's placement for jobs nobody chose one for.
func (s *Service) JobPlacement(ctx context.Context, projectID pgtype.UUID) (string, error) {
	c, err := s.projectConfig(ctx, projectID)
	if err != nil {
		return "", err
	}
	return c.Placement(), nil
}

// PolicyFor resolves a role: the role's entry over the policy default, over the autopilot
// backend, over the server default. A role that switches backend drops the default model,
// which belongs to the other backend.
func (c ProjectConfig) PolicyFor(role, defaultBackend string) RolePolicy {
	out := RolePolicy{Backend: defaultBackend}
	if c.Autopilot != nil && c.Autopilot.Backend != "" {
		out.Backend = c.Autopilot.Backend
	}
	if c.Policy == nil {
		return out
	}
	out = out.Over(c.Policy.Default)
	if role != "" {
		out = out.Over(c.Policy.Roles[role])
	}
	return out
}

// Over is p with what src says on top of it. A src that switches backend drops p's model, which
// belongs to the other backend.
func (p RolePolicy) Over(src RolePolicy) RolePolicy {
	if src.Backend != "" && src.Backend != p.Backend {
		p.Backend, p.Model = src.Backend, ""
	}
	if src.Model != "" {
		p.Model = src.Model
	}
	if src.MaxCostUSD > 0 {
		p.MaxCostUSD = src.MaxCostUSD
	}
	return p
}

// PolicyForTurn resolves an agent's turn: the role's policy, then the tier's (a cheaper model
// for chat), then the agent's own, which a person set for that agent alone.
func (c ProjectConfig) PolicyForTurn(role, tier string, own RolePolicy, defaultBackend string) RolePolicy {
	out := c.PolicyFor(role, defaultBackend)
	if c.Policy != nil && tier != "" {
		out = out.Over(c.Policy.Tiers[tier])
	}
	return out.Over(own)
}

// TurnPolicy is PolicyForTurn for a project.
func (s *Service) TurnPolicy(ctx context.Context, projectID pgtype.UUID, role, tier string, own RolePolicy, defaultBackend string) (RolePolicy, error) {
	c, err := s.projectConfig(ctx, projectID)
	if err != nil {
		return RolePolicy{}, err
	}
	return c.PolicyForTurn(role, tier, own, defaultBackend), nil
}

// JobPolicy is the resolved policy for a role in a project.
func (s *Service) JobPolicy(ctx context.Context, projectID pgtype.UUID, role, defaultBackend string) (RolePolicy, error) {
	c, err := s.projectConfig(ctx, projectID)
	if err != nil {
		return RolePolicy{}, err
	}
	return c.PolicyFor(role, defaultBackend), nil
}

// Budget returns the project's daily budget (0 = none) and what it has spent since the
// start of the current UTC day.
func (s *Service) Budget(ctx context.Context, projectID pgtype.UUID, now time.Time) (limit, spent float64, err error) {
	c, err := s.projectConfig(ctx, projectID)
	if err != nil || c.Policy == nil || c.Policy.DailyBudgetUSD <= 0 {
		return 0, 0, err
	}
	day := now.UTC().Truncate(24 * time.Hour)
	spent, err = db.New(s.pool).ProjectCostSince(ctx, db.ProjectCostSinceParams{ProjectID: projectID, Since: pgtype.Timestamptz{Time: day, Valid: true}})
	return c.Policy.DailyBudgetUSD, spent, err
}
