package process

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// RolePolicy says which backend and model run a role and what one job may cost.
type RolePolicy struct {
	Backend    string  `json:"backend,omitempty"`
	Model      string  `json:"model,omitempty"`
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
}

// Policy is the project's choice of backends and models per role, and its daily spend limit.
// There is no automatic fallback: a job waits for a runner with its backend logged in rather
// than running somewhere the owner did not choose.
type Policy struct {
	Default        RolePolicy            `json:"default"`
	Roles          map[string]RolePolicy `json:"roles"`
	DailyBudgetUSD float64               `json:"daily_budget_usd"`
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
	merge := func(src RolePolicy) {
		if src.Backend != "" && src.Backend != out.Backend {
			out.Backend, out.Model = src.Backend, ""
		}
		if src.Model != "" {
			out.Model = src.Model
		}
		if src.MaxCostUSD > 0 {
			out.MaxCostUSD = src.MaxCostUSD
		}
	}
	merge(c.Policy.Default)
	if role != "" {
		merge(c.Policy.Roles[role])
	}
	return out
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
