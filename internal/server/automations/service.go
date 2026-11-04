package automations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

var (
	// ErrUnknown answers a key the catalogue does not have.
	ErrUnknown = errors.New("no such automation")
	// ErrConsentRequired answers switching on an automation that puts agents to work without
	// saying that they may work without asking.
	ErrConsentRequired = errors.New("switching this automation on allows agents to work without asking on the tasks it creates; say so with allowAgents")
	// ErrUnavailable answers switching on an automation the project cannot run yet.
	ErrUnavailable = errors.New("this project cannot run the automation yet")
	// ErrInvalidSettings answers settings that do not fit the automation.
	ErrInvalidSettings = errors.New("invalid automation settings")
)

// Service stores each project's automations and runs their triggers.
type Service struct {
	pool *pgxpool.Pool
	proc *process.Service
	hub  *events.Hub
	log  *slog.Logger
	now  func() time.Time
}

// New builds the service. hub may be nil, which only means a failed job is not reported on the
// automation's card.
func New(pool *pgxpool.Pool, proc *process.Service, hub *events.Hub, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, proc: proc, hub: hub, log: log, now: time.Now}
}

// View is one automation as a project sees it: the catalogue entry with the project's state.
type View struct {
	Definition
	Enabled  bool
	Settings Settings
	// Available is false while the project lacks what the automation needs; Unavailable says
	// what to do about it.
	Available   bool
	Unavailable string
	EnabledBy   pgtype.UUID
	EnabledName string
	EnabledAt   pgtype.Timestamptz
	LastRunAt   pgtype.Timestamptz
	LastError   string
	LastTaskID  pgtype.UUID
	// NextRunAt is when a weekly automation that is on runs next.
	NextRunAt pgtype.Timestamptz
}

// List is the whole catalogue with the project's state, in catalogue order.
func (s *Service) List(ctx context.Context, projectID pgtype.UUID) ([]View, error) {
	q := db.New(s.pool)
	rows, err := q.ListProjectAutomations(ctx, projectID)
	if err != nil {
		return nil, err
	}
	byKey := map[string]db.ProjectAutomation{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	latest, err := q.LatestAutomationTasks(ctx, projectID)
	if err != nil {
		return nil, err
	}
	lastTask := map[string]pgtype.UUID{}
	for _, l := range latest {
		lastTask[l.Automation] = l.TaskID
	}
	have, err := s.have(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	names := map[[16]byte]string{}
	out := make([]View, 0, len(Catalogue))
	for _, d := range Catalogue {
		v, err := s.view(ctx, q, d, byKey[d.Key], have, names)
		if err != nil {
			return nil, err
		}
		v.LastTaskID = lastTask[d.Key]
		out = append(out, v)
	}
	return out, nil
}

// Get is one automation of a project.
func (s *Service) Get(ctx context.Context, projectID pgtype.UUID, key string) (View, error) {
	views, err := s.List(ctx, projectID)
	if err != nil {
		return View{}, err
	}
	for _, v := range views {
		if v.Key == key {
			return v, nil
		}
	}
	return View{}, ErrUnknown
}

func (s *Service) view(ctx context.Context, q *db.Queries, d Definition, row db.ProjectAutomation, have map[string]bool,
	names map[[16]byte]string) (View, error) {
	v := View{Definition: d, Enabled: row.Enabled, EnabledBy: row.EnabledBy, EnabledAt: row.EnabledAt, LastRunAt: row.LastRunAt,
		LastError: row.LastError}
	var stored Settings
	_ = json.Unmarshal(row.Settings, &stored)
	v.Settings, _ = d.Normalize(stored)
	v.Available, v.Unavailable = available(d, have)
	if row.EnabledBy.Valid {
		name, ok := names[row.EnabledBy.Bytes]
		if !ok {
			if u, err := q.GetUser(ctx, row.EnabledBy); err == nil {
				name = u.Name
			}
			names[row.EnabledBy.Bytes] = name
		}
		v.EnabledName = name
	}
	if d.Weekly() && row.Enabled && row.ProjectID.Valid {
		next, err := s.nextRun(ctx, q, row, d.Day(v.Settings))
		if err != nil {
			return v, err
		}
		v.NextRunAt = pgtype.Timestamptz{Time: next, Valid: true}
	}
	return v, nil
}

// have says which of the needs the project meets.
func (s *Service) have(ctx context.Context, q *db.Queries, projectID pgtype.UUID) (map[string]bool, error) {
	caps, err := q.ProjectCapabilities(ctx, projectID)
	if err != nil {
		return nil, err
	}
	repos, err := q.CountProjectRepos(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return map[string]bool{
		NeedsNothing:    true,
		NeedsGitHub:     slices.Contains(caps, "vcs"),
		NeedsMonitoring: slices.Contains(caps, "monitoring"),
		NeedsRepository: repos > 0,
	}, nil
}

func available(d Definition, have map[string]bool) (bool, string) {
	if have[d.Needs] {
		return true, ""
	}
	switch d.Needs {
	case NeedsGitHub:
		return false, "Switch GitHub on in this project first."
	case NeedsMonitoring:
		return false, "Switch on a monitoring tool, Honeybadger or Sentry, in this project first."
	case NeedsRepository:
		return false, "Add the project's repository first."
	}
	return false, "This project cannot run it yet."
}

// Change is what a person asks of an automation.
type Change struct {
	Enabled bool
	// Settings replaces the stored ones when given.
	Settings *Settings
	// AllowAgents is the person saying agents may work without asking on the tasks it creates.
	// Switching on one that uses agents requires it.
	AllowAgents bool
}

// Set switches an automation on or off, or changes its settings. The caller has checked the
// person is a project admin.
func (s *Service) Set(ctx context.Context, projectID pgtype.UUID, key string, c Change, by pgtype.UUID) (View, error) {
	d, ok := Lookup(key)
	if !ok {
		return View{}, ErrUnknown
	}
	q := db.New(s.pool)
	var settings Settings
	wasOn := false
	if row, err := q.GetProjectAutomation(ctx, db.GetProjectAutomationParams{ProjectID: projectID, Key: key}); err == nil {
		_ = json.Unmarshal(row.Settings, &settings)
		wasOn = row.Enabled && row.EnabledBy.Valid
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return View{}, err
	}
	if c.Settings != nil {
		settings = *c.Settings
	}
	settings, err := d.Normalize(settings)
	if err != nil {
		return View{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	raw, _ := json.Marshal(settings)
	if c.Enabled {
		// Changing the day of one already on keeps the consent it was switched on with, and the
		// person who gave it; only switching it on asks.
		if d.UsesAgents && !c.AllowAgents && !wasOn {
			return View{}, ErrConsentRequired
		}
		if !by.Valid {
			return View{}, process.ErrConsentNeedsPerson
		}
		have, err := s.have(ctx, q, projectID)
		if err != nil {
			return View{}, err
		}
		if ok, why := available(d, have); !ok {
			return View{}, fmt.Errorf("%w: %s", ErrUnavailable, why)
		}
		if _, err := q.EnableProjectAutomation(ctx, db.EnableProjectAutomationParams{ProjectID: projectID, Key: key, Settings: raw, EnabledBy: by}); err != nil {
			return View{}, err
		}
	} else if _, err := q.DisableProjectAutomation(ctx, db.DisableProjectAutomationParams{ProjectID: projectID, Key: key, Settings: raw}); err != nil {
		return View{}, err
	}
	payload, _ := json.Marshal(map[string]any{"project": uuidString(projectID), "key": key, "enabled": c.Enabled})
	if _, err := q.InsertEvent(ctx, db.InsertEventParams{Type: "automation.changed", Aggregate: "project", AggregateID: projectID, Payload: payload}); err != nil {
		return View{}, err
	}
	return s.Get(ctx, projectID, key)
}

// enabled is the project's row for an automation that is switched on, or false.
func (s *Service) enabled(ctx context.Context, q *db.Queries, projectID pgtype.UUID, key string) (db.ProjectAutomation, bool, error) {
	row, err := q.GetProjectAutomation(ctx, db.GetProjectAutomationParams{ProjectID: projectID, Key: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	if !row.Enabled {
		return row, false, nil
	}
	if !row.EnabledBy.Valid {
		// The person who switched it on is gone, and with them the consent: nobody is left to
		// name on the tasks it would create.
		s.fail(ctx, q, projectID, key, "Nobody is named as having switched this on any more. Switch it off and on again.")
		return row, false, nil
	}
	return row, true, nil
}

// fail records why a run did not happen, for the automation's card.
func (s *Service) fail(ctx context.Context, q *db.Queries, projectID pgtype.UUID, key, why string) {
	if err := q.SetAutomationError(ctx, db.SetAutomationErrorParams{ProjectID: projectID, Key: key, LastError: why}); err != nil {
		s.log.Warn("automation error", "automation", key, "err", err)
	}
}

// errAlreadyRan rolls back a task whose subject already had its run.
var errAlreadyRan = errors.New("already ran")

// claim records the run for a subject inside the task's transaction, or rolls it back when the
// subject already had one.
func claim(ctx context.Context, projectID pgtype.UUID, key, subject string) func(q *db.Queries, t db.Task) error {
	return func(q *db.Queries, t db.Task) error {
		run, err := q.ClaimAutomationRun(ctx, db.ClaimAutomationRunParams{ProjectID: projectID, Automation: key, Subject: subject})
		if errors.Is(err, pgx.ErrNoRows) {
			return errAlreadyRan
		}
		if err != nil {
			return err
		}
		if err := q.SetAutomationRunTask(ctx, db.SetAutomationRunTaskParams{ID: run.ID, TaskID: t.ID}); err != nil {
			return err
		}
		return q.RecordAutomationRun(ctx, db.RecordAutomationRunParams{ProjectID: projectID, Key: key})
	}
}

func automated(d Definition, row db.ProjectAutomation, inTx func(q *db.Queries, t db.Task) error) *process.Automated {
	return &process.Automated{Key: d.Key, Title: d.Title, AllowAgents: d.UsesAgents, AllowedBy: row.EnabledBy, InTx: inTx}
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
