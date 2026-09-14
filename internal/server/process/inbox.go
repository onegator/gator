package process

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store/db"
)

// DecisionReason says why a task sits in the inbox.
type DecisionReason string

const (
	ReasonApproval            DecisionReason = "approval"
	ReasonBlocked             DecisionReason = "blocked"
	ReasonRequirementsChanged DecisionReason = "requirements_changed"
	ReasonIdea                DecisionReason = "idea"
	ReasonJobFailed           DecisionReason = "job_failed"
)

// Decision is one inbox row: a task that needs a human now.
type Decision struct {
	Task         db.Task
	Gate         db.Gate
	Phase        Phase
	Reason       DecisionReason
	WaitingSince time.Time
}

// Inbox lists open tasks whose current phase waits on a person: a human-gated phase without
// approval, a blocked task, or one whose requirements changed. Most urgent first.
func (s *Service) Inbox(ctx context.Context, projectID pgtype.UUID) ([]Decision, error) {
	q := db.New(s.pool)
	rows, err := q.ListOpenTasksWithGates(ctx, projectID)
	if err != nil {
		return nil, err
	}
	jobRows, err := q.ListCurrentPhaseJobs(ctx)
	if err != nil {
		return nil, err
	}
	jobStatus := map[[16]byte]string{}
	for _, j := range jobRows {
		jobStatus[j.TaskID.Bytes] = j.Status
	}
	machines := map[string]*Machine{}
	var out []Decision
	for _, r := range rows {
		key := uuidString(r.Task.ProjectID) + "/" + r.Task.Kind
		m, ok := machines[key]
		if !ok {
			m, err = s.machine(ctx, r.Task.ProjectID, r.Task.Kind)
			if err != nil {
				return nil, err
			}
			machines[key] = m
		}
		p, err := m.Phase(r.Task.Phase)
		if err != nil {
			continue // phase deactivated by config change; surfaced elsewhere
		}
		reason, ok := decisionReason(r.Task, r.Gate, p, jobStatus[r.Task.ID.Bytes])
		if !ok {
			continue
		}
		out = append(out, Decision{Task: r.Task, Gate: r.Gate, Phase: p, Reason: reason, WaitingSince: r.Task.PhaseEnteredAt.Time})
	}
	return out, nil
}

func decisionReason(t db.Task, g db.Gate, p Phase, jobStatus string) (DecisionReason, bool) {
	switch {
	case t.BlockedReason != nil:
		return ReasonBlocked, true
	case t.RequirementsChanged:
		return ReasonRequirementsChanged, true
	case jobStatus == "queued" || jobStatus == "leased" || jobStatus == "running" || jobStatus == "stalled":
		return "", false // an agent is on it; asking a person now would be premature
	case jobStatus == "failed" || jobStatus == "stopped":
		return ReasonJobFailed, true
	case p.Name == "idea":
		return ReasonIdea, true
	case (p.Gate == GateHuman || p.Gate == GateBoth) && !g.HumanApprovedAt.Valid:
		return ReasonApproval, true
	}
	return "", false
}

// Detail bundles a task with its gate and the phases of its template.
type Detail struct {
	Task   db.Task
	Gate   db.Gate
	Phase  Phase
	Phases []Phase
}

// Detail loads a task with gate and template context.
func (s *Service) Detail(ctx context.Context, taskID pgtype.UUID) (Detail, error) {
	q := db.New(s.pool)
	t, err := q.GetTask(ctx, taskID)
	if err != nil {
		return Detail{}, err
	}
	g, err := q.GetGate(ctx, db.GetGateParams{TaskID: taskID, Phase: t.Phase})
	if err != nil {
		return Detail{}, err
	}
	m, err := s.machine(ctx, t.ProjectID, t.Kind)
	if err != nil {
		return Detail{}, err
	}
	p, _ := m.Phase(t.Phase)
	return Detail{Task: t, Gate: g, Phase: p, Phases: m.Phases()}, nil
}

// Transitions returns the audit trail of a task.
func (s *Service) Transitions(ctx context.Context, taskID pgtype.UUID) ([]db.PhaseTransition, error) {
	return db.New(s.pool).ListPhaseTransitions(ctx, taskID)
}

// Tasks lists open tasks of a project.
func (s *Service) Tasks(ctx context.Context, projectID pgtype.UUID) ([]db.Task, error) {
	return db.New(s.pool).ListOpenTasksByProject(ctx, projectID)
}

// DBTemplates resolves templates from defaults plus the project's process_config.
// process_config shape: {"templates": {"<kind>": <Template>}}.
type DBTemplates struct {
	Pool     *pgxpool.Pool
	Defaults Catalog
}

// Catalog implements TemplateResolver.
func (d DBTemplates) Catalog(ctx context.Context, projectID pgtype.UUID) (Catalog, error) {
	p, err := db.New(d.Pool).GetProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("project: %w", err)
	}
	cfg, err := ParseProjectConfig(p.ProcessConfig)
	if err != nil {
		return nil, err
	}
	overrides := Catalog{}
	for k, t := range cfg.Templates {
		if t.Kind == "" {
			t.Kind = k
		}
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("process_config template %q: %w", k, err)
		}
		overrides[k] = t
	}
	return d.Defaults.Override(overrides), nil
}
