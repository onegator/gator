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
	ReasonNoRunner            DecisionReason = "no_runner"
	// ReasonExternal is work raised from outside the workspace, waiting to be let in.
	ReasonExternal DecisionReason = "external"
)

// Decision is one inbox row: a task that needs a human now.
type Decision struct {
	Task         db.Task
	Gate         db.Gate
	Phase        Phase
	Reason       DecisionReason
	WaitingSince time.Time
	Rollbacks    int // times this task was sent back into its current phase
}

// InboxFilter narrows the inbox to one project, to the tasks handed to one person, or both.
type InboxFilter struct {
	ProjectID pgtype.UUID
	OwnerID   pgtype.UUID // set: only tasks whose owner is this user
}

// Inbox lists open tasks whose current phase waits on a person: a human-gated phase without
// approval, a blocked task, or one whose requirements changed. Most urgent first.
func (s *Service) Inbox(ctx context.Context, f InboxFilter) ([]Decision, error) {
	q := db.New(s.pool)
	rows, err := q.ListOpenTasksWithGates(ctx, f.ProjectID)
	if err != nil {
		return nil, err
	}
	rollbackRows, err := q.ListRollbackCounts(ctx)
	if err != nil {
		return nil, err
	}
	type phaseKey struct {
		task  [16]byte
		phase string
	}
	rollbacks := map[phaseKey]int{}
	for _, r := range rollbackRows {
		rollbacks[phaseKey{r.TaskID.Bytes, r.ToPhase}] = int(r.Rollbacks)
	}
	jobRows, err := q.ListCurrentPhaseJobs(ctx)
	if err != nil {
		return nil, err
	}
	jobStatus := map[[16]byte]string{}
	for _, j := range jobRows {
		jobStatus[j.TaskID.Bytes] = j.Status
	}
	orphanRows, err := q.ListUnassignableTaskIDs(ctx)
	if err != nil {
		return nil, err
	}
	orphaned := map[[16]byte]bool{}
	for _, id := range orphanRows {
		orphaned[id.Bytes] = true
	}
	// A campaign is one decision. Its children's approvals belong to that decision, so they
	// stay out of the inbox — unless one is stuck, which is the case a person must see.
	childRows, err := q.ListCampaignChildIDs(ctx)
	if err != nil {
		return nil, err
	}
	campaignChild := map[[16]byte]bool{}
	for _, id := range childRows {
		campaignChild[id.Bytes] = true
	}
	machines := map[string]*Machine{}
	var out []Decision
	for _, r := range rows {
		if f.OwnerID.Valid && !ownedBy(r.Task, f.OwnerID) {
			continue
		}
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
		reason, ok := decisionReason(r.Task, r.Gate, p, jobStatus[r.Task.ID.Bytes], orphaned[r.Task.ID.Bytes])
		if !ok {
			continue
		}
		if campaignChild[r.Task.ID.Bytes] && !stuck(reason) {
			continue
		}
		out = append(out, Decision{Task: r.Task, Gate: r.Gate, Phase: p, Reason: reason,
			WaitingSince: r.Task.PhaseEnteredAt.Time, Rollbacks: rollbacks[phaseKey{r.Task.ID.Bytes, r.Task.Phase}]})
	}
	return out, nil
}

// stuck says whether a reason means the task cannot move on its own. A campaign hides its
// children's routine approvals, never their problems.
func stuck(r DecisionReason) bool {
	switch r {
	case ReasonBlocked, ReasonJobFailed, ReasonNoRunner, ReasonRequirementsChanged, ReasonExternal:
		return true
	}
	return false
}

// ownedBy reports whether the task was handed to this person.
func ownedBy(t db.Task, userID pgtype.UUID) bool {
	return t.OwnerKind != nil && *t.OwnerKind == string(ActorUser) && t.OwnerID == userID
}

func decisionReason(t db.Task, g db.Gate, p Phase, jobStatus string, unassignable bool) (DecisionReason, bool) {
	switch {
	case t.BlockedReason != nil:
		return ReasonBlocked, true
	case t.Origin == "external" && !t.AdmittedAt.Valid:
		// Somebody outside wrote this. Nothing runs on it until a person says it may.
		return ReasonExternal, true
	case t.RequirementsChanged:
		return ReasonRequirementsChanged, true
	case unassignable:
		// The job is queued, so the case below would call this premature — but no runner can
		// take it, and waiting for an agent that cannot come is how a task waits forever.
		return ReasonNoRunner, true
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

// DBTemplates resolves templates in the order a person would expect: the project's own
// overrides, then the workspace's, then the defaults in the binary. The middle step is what
// lets a team have one way of working without pasting it into every project.
// process_config shape, in both places: {"templates": {"<kind>": <Template>}}.
type DBTemplates struct {
	Pool     *pgxpool.Pool
	Defaults Catalog
}

// Catalog implements TemplateResolver.
func (d DBTemplates) Catalog(ctx context.Context, projectID pgtype.UUID) (Catalog, error) {
	catalog, _, err := d.catalogWithSources(ctx, projectID)
	return catalog, err
}

// TemplateSource says where the template in force for a kind came from, so the screen that
// shows a process can say what it is that a project would be overriding.
type TemplateSource string

const (
	SourceProject   TemplateSource = "project"
	SourceWorkspace TemplateSource = "workspace"
	SourceDefault   TemplateSource = "default"
)

// CatalogWithSources is Catalog plus where each kind's template came from.
func (d DBTemplates) CatalogWithSources(ctx context.Context, projectID pgtype.UUID) (Catalog, map[string]TemplateSource, error) {
	return d.catalogWithSources(ctx, projectID)
}

func (d DBTemplates) catalogWithSources(ctx context.Context, projectID pgtype.UUID) (Catalog, map[string]TemplateSource, error) {
	q := db.New(d.Pool)
	p, err := q.GetProject(ctx, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("project: %w", err)
	}
	ws, err := q.GetWorkspaceSettings(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("workspace settings: %w", err)
	}
	sources := map[string]TemplateSource{}
	for kind := range d.Defaults {
		sources[kind] = SourceDefault
	}
	workspaceOverrides, err := templateOverrides(ws.ProcessConfig, "workspace process")
	if err != nil {
		return nil, nil, err
	}
	for kind := range workspaceOverrides {
		sources[kind] = SourceWorkspace
	}
	projectOverrides, err := templateOverrides(p.ProcessConfig, "process_config")
	if err != nil {
		return nil, nil, err
	}
	for kind := range projectOverrides {
		sources[kind] = SourceProject
	}
	return d.Defaults.Override(workspaceOverrides).Override(projectOverrides), sources, nil
}

// templateOverrides reads the templates out of one process_config document. An invalid template
// is refused here rather than at the moment a task tries to use it.
func templateOverrides(raw []byte, where string) (Catalog, error) {
	cfg, err := ParseProjectConfig(raw)
	if err != nil {
		return nil, err
	}
	out := Catalog{}
	for k, t := range cfg.Templates {
		if t.Kind == "" {
			t.Kind = k
		}
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("%s template %q: %w", where, k, err)
		}
		out[k] = t
	}
	return out, nil
}
