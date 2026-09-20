// Package quality scores what a project has said about itself. A scorecard is a set of rules
// asked about each component on a schedule: some the core can answer from what it already
// knows, the rest come from plugins with the `quality` capability. The point is not the number
// but the direction, and a slip that nobody notices is the same as no scorecard at all.
package quality

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// Check is one rule's answer about one component.
type Check struct {
	// Component is the component's key; empty means the project as a whole.
	Component string
	Rule      string
	Source    string // "builtin" or the plugin's name
	Status    string // pass | fail | unknown
	Detail    string
	Weight    int
}

// Asker is the plugin host. The interface keeps this package off plugins.
type Asker interface {
	// QualityChecks asks every plugin of the project that offers the capability. A plugin that
	// fails is left out rather than failing the sweep: one broken plugin must not make a whole
	// project look bad.
	QualityChecks(ctx context.Context, projectID pgtype.UUID, components []string) []Check
}

// Service runs scorecards.
type Service struct {
	pool    *pgxpool.Pool
	proc    *process.Service
	plugins Asker
	log     *slog.Logger
}

func New(pool *pgxpool.Pool, proc *process.Service, plugins Asker, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, proc: proc, plugins: plugins, log: log}
}

// Policy is what a project says about its scorecard, read from process_config.
type Policy struct {
	// Enabled is false by default: a scorecard nobody asked for is noise.
	Enabled bool `json:"enabled"`
	// Threshold is the share of the possible points below which a component counts as slipped.
	// Zero means 0.8.
	Threshold float64 `json:"threshold"`
	// OnRegression is "task" (default) or "none": some teams want the numbers without a task.
	OnRegression string `json:"on_regression"`
}

const defaultThreshold = 0.8

// SweepAll scores every project that has named its components. Returns how many it scored.
func (s *Service) SweepAll(ctx context.Context) (int, error) {
	q := db.New(s.pool)
	ids, err := q.ProjectsWithComponents(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		switch err := s.Sweep(ctx, id); {
		case err != nil:
			s.log.Warn("scoring a project", "project", id, "err", err)
		default:
			n++
		}
	}
	return n, nil
}

// Sweep scores one project: asks every rule, stores the answers and the score, and opens or
// closes the task a slipped component deserves.
func (s *Service) Sweep(ctx context.Context, projectID pgtype.UUID) error {
	policy, err := s.policy(ctx, projectID)
	if err != nil {
		return err
	}
	if !policy.Enabled {
		return nil
	}
	q := db.New(s.pool)
	components, err := q.ListComponents(ctx, projectID)
	if err != nil {
		return err
	}
	if len(components) == 0 {
		return nil
	}
	byKey := make(map[string]db.Component, len(components))
	keys := make([]string, 0, len(components))
	for _, c := range components {
		byKey[c.Key] = c
		keys = append(keys, c.Key)
	}

	checks := builtinChecks(components)
	if s.plugins != nil {
		checks = append(checks, s.plugins.QualityChecks(ctx, projectID, keys)...)
	}

	// earned and possible per component, so each part gets its own score rather than the
	// project's average hiding the one part that is falling apart.
	type tally struct{ earned, possible int }
	tallies := map[string]*tally{}
	for _, c := range checks {
		if c.Weight <= 0 {
			c.Weight = 1
		}
		if c.Status != "pass" && c.Status != "fail" && c.Status != "unknown" {
			s.log.Warn("ignoring a check with an unknown status", "rule", c.Rule, "status", c.Status)
			continue
		}
		var componentID pgtype.UUID
		if c.Component != "" {
			comp, ok := byKey[c.Component]
			if !ok {
				s.log.Warn("a check about a component this project has not named", "rule", c.Rule, "component", c.Component)
				continue
			}
			componentID = comp.ID
		}
		row, err := q.RecordQualityCheck(ctx, db.RecordQualityCheckParams{
			ProjectID: projectID, ComponentID: componentID, Rule: c.Rule, Source: c.Source,
			Status: c.Status, Detail: c.Detail, Weight: int32(c.Weight)})
		if err != nil {
			return err
		}
		t := tallies[c.Component]
		if t == nil {
			t = &tally{}
			tallies[c.Component] = t
		}
		// An unknown answer counts against nothing: a plugin that could not reach its service
		// says nothing about the code, and scoring it as a failure would punish an outage.
		if c.Status == "unknown" {
			continue
		}
		t.possible += c.Weight
		if c.Status == "pass" {
			t.earned += c.Weight
			if err := s.onRecovered(ctx, q, row); err != nil {
				return err
			}
		}
	}

	for key, t := range tallies {
		var componentID pgtype.UUID
		if comp, ok := byKey[key]; ok {
			componentID = comp.ID
		}
		if _, err := q.RecordQualityScore(ctx, db.RecordQualityScoreParams{
			ProjectID: projectID, ComponentID: componentID,
			Earned: int32(t.earned), Possible: int32(t.possible)}); err != nil {
			return err
		}
		if t.possible == 0 || policy.OnRegression == "none" {
			continue
		}
		if float64(t.earned)/float64(t.possible) >= policy.threshold() {
			continue
		}
		if err := s.openRegression(ctx, q, projectID, componentID, key, t.earned, t.possible); err != nil {
			return err
		}
	}
	return nil
}

func (p Policy) threshold() float64 {
	if p.Threshold <= 0 || p.Threshold > 1 {
		return defaultThreshold
	}
	return p.Threshold
}

// onRecovered closes the task a failing rule opened, once the rule passes again. Leaving it
// open would be a lie about the state of the project, and asking a person to tidy it up is the
// chore this whole product exists to spare them.
func (s *Service) onRecovered(ctx context.Context, q *db.Queries, row db.RecordQualityCheckRow) error {
	if row.PreviousStatus != "fail" || !row.PreviousTaskID.Valid {
		return nil
	}
	if _, err := s.proc.Close(ctx, row.PreviousTaskID, process.Actor{Kind: process.ActorSystem},
		"quality rule "+row.Rule+" passes again"); err != nil {
		s.log.Warn("closing a recovered quality task", "task", row.PreviousTaskID, "err", err)
	}
	return nil
}

// openRegression opens one chore for a component that has slipped, and only one: a scorecard
// that files a task every hour is a scorecard people turn off.
func (s *Service) openRegression(ctx context.Context, q *db.Queries, projectID, componentID pgtype.UUID, key string, earned, possible int) error {
	failing, err := q.ListQualityChecks(ctx, projectID)
	if err != nil {
		return err
	}
	var open []db.QualityCheck
	for _, c := range failing {
		if c.Status == "fail" && c.ComponentID == componentID {
			if c.TaskID.Valid {
				return nil // this component's slip is already somebody's problem
			}
			open = append(open, c)
		}
	}
	if len(open) == 0 {
		return nil
	}
	title := fmt.Sprintf("Quality slipped: %s (%d/%d)", nameFor(key), earned, possible)
	task, err := s.proc.Create(ctx, process.CreateParams{
		ProjectID: projectID, Kind: "chore", Title: title,
		Description: regressionDescription(open), Urgency: 3,
	}, process.Actor{Kind: process.ActorSystem})
	if err != nil {
		return err
	}
	for _, c := range open {
		if err := q.SetQualityCheckTask(ctx, db.SetQualityCheckTaskParams{ID: c.ID, TaskID: task.ID}); err != nil {
			return err
		}
	}
	return nil
}

func nameFor(key string) string {
	if key == "" {
		return "the project"
	}
	return key
}

// regressionDescription says which rules failed and since when, so the task can be worked on
// without going to look the numbers up somewhere else.
func regressionDescription(checks []db.QualityCheck) string {
	var b strings.Builder
	b.WriteString("These rules are failing:\n\n")
	for _, c := range checks {
		b.WriteString("- **" + c.Rule + "**")
		if c.Detail != "" {
			b.WriteString(" — " + c.Detail)
		}
		if c.FailingSince.Valid {
			b.WriteString(" (since " + c.FailingSince.Time.Format(time.DateOnly) + ")")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nThe task closes by itself once they pass again.")
	return b.String()
}
