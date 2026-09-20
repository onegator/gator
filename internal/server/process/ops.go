package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/server/telemetry"
	"go.opentelemetry.io/otel/metric"
)

var (
	ErrGateNotSatisfied = errors.New("gate not satisfied")
	ErrTaskBlocked      = errors.New("task is blocked")
	ErrTaskClosed       = errors.New("task is closed")
)

// ActorKind identifies who performs an operation.
type ActorKind string

const (
	ActorUser   ActorKind = "user"
	ActorRunner ActorKind = "runner"
	ActorPlugin ActorKind = "plugin"
	ActorSystem ActorKind = "system"
)

// Actor is the subject recorded on every transition.
type Actor struct {
	Kind ActorKind
	ID   pgtype.UUID // zero for system
}

// Check is one automated or human verdict on a gate.
type Check struct {
	Name   string `json:"name"`
	Source string `json:"source"` // "plugin:<name>" | "human" | "system"
	Status string `json:"status"` // "pass" | "fail" | "pending"
	Detail string `json:"detail,omitempty"`
}

// CapabilityResolver answers which plugin capabilities a project has enabled.
// The plugins package implements it; process only depends on the interface.
type CapabilityResolver interface {
	Capabilities(ctx context.Context, projectID pgtype.UUID) ([]string, error)
}

// TemplateResolver returns the effective catalog for a project (defaults + overrides).
type TemplateResolver interface {
	Catalog(ctx context.Context, projectID pgtype.UUID) (Catalog, error)
}

// Service performs every mutation of a task. All methods run in one transaction that
// writes the task, a phase_transitions row and an outbox event together.
type Service struct {
	pool      *pgxpool.Pool
	templates TemplateResolver
	caps      CapabilityResolver
}

// NewService wires the service.
func NewService(pool *pgxpool.Pool, templates TemplateResolver, caps CapabilityResolver) *Service {
	return &Service{pool: pool, templates: templates, caps: caps}
}

// StaticCatalog is a TemplateResolver that ignores the project. Useful for tests and
// until per-project overrides are wired.
type StaticCatalog Catalog

// Catalog implements TemplateResolver.
func (s StaticCatalog) Catalog(context.Context, pgtype.UUID) (Catalog, error) { return Catalog(s), nil }

// StaticCapabilities is a CapabilityResolver returning a fixed list.
type StaticCapabilities []string

// Capabilities implements CapabilityResolver.
func (s StaticCapabilities) Capabilities(context.Context, pgtype.UUID) ([]string, error) {
	return s, nil
}

func (s *Service) machine(ctx context.Context, projectID pgtype.UUID, kind string) (*Machine, error) {
	cat, err := s.templates.Catalog(ctx, projectID)
	if err != nil {
		return nil, err
	}
	tpl, ok := cat[kind]
	if !ok {
		return nil, fmt.Errorf("no template for kind %q", kind)
	}
	caps, err := s.caps.Capabilities(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return NewMachine(tpl, caps)
}

// CreateParams describes a new task.
type CreateParams struct {
	ProjectID    pgtype.UUID
	Kind         string
	Title        string
	Description  string
	Urgency      int16
	SourceTaskID pgtype.UUID
}

// Create inserts a task in the first phase of its template.
func (s *Service) Create(ctx context.Context, p CreateParams, actor Actor) (db.Task, error) {
	m, err := s.machine(ctx, p.ProjectID, p.Kind)
	if err != nil {
		return db.Task{}, err
	}
	first := m.First()
	if p.Urgency == 0 {
		p.Urgency = 3
	}
	var task db.Task
	err = s.tx(ctx, func(q *db.Queries) error {
		var err error
		task, err = q.CreateTask(ctx, db.CreateTaskParams{
			ProjectID: p.ProjectID, Kind: p.Kind, Title: p.Title, Description: p.Description, Phase: first.Name,
			Urgency: p.Urgency, SourceTaskID: p.SourceTaskID,
		})
		if err != nil {
			return err
		}
		if err := q.UpsertGate(ctx, db.UpsertGateParams{TaskID: task.ID, Phase: first.Name}); err != nil {
			return err
		}
		if err := s.record(ctx, q, task.ID, nil, first.Name, "create", actor, "", nil); err != nil {
			return err
		}
		return s.emit(ctx, q, "task.created", task.ID, map[string]any{"kind": p.Kind, "phase": first.Name})
	})
	return task, err
}

// Advance moves a task to the next phase if the current gate is satisfied.
func (s *Service) Advance(ctx context.Context, taskID pgtype.UUID, actor Actor, reason string) (db.Task, error) {
	var out db.Task
	err := s.tx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, taskID)
		if err != nil {
			return err
		}
		if task.ClosedAt.Valid {
			return ErrTaskClosed
		}
		if task.BlockedReason != nil {
			return fmt.Errorf("%w: %s", ErrTaskBlocked, *task.BlockedReason)
		}
		m, err := s.machine(ctx, task.ProjectID, task.Kind)
		if err != nil {
			return err
		}
		cur, err := m.Phase(task.Phase)
		if err != nil {
			return err
		}
		gate, err := q.GetGate(ctx, db.GetGateParams{TaskID: taskID, Phase: task.Phase})
		if err != nil {
			return fmt.Errorf("gate: %w", err)
		}
		if err := gateSatisfied(cur, gate); err != nil {
			return err
		}
		if m.IsTerminal(task.Phase) {
			if err := q.SetTaskPhase(ctx, db.SetTaskPhaseParams{ID: taskID, Phase: task.Phase, Close: true}); err != nil {
				return err
			}
			if err := s.record(ctx, q, taskID, &task.Phase, task.Phase, "close", actor, reason, nil); err != nil {
				return err
			}
			if err := s.emit(ctx, q, "task.closed", taskID, map[string]any{"phase": task.Phase}); err != nil {
				return err
			}
		} else {
			next, err := m.Next(task.Phase)
			if err != nil {
				return err
			}
			observePhaseTime(ctx, task)
			if err := q.SetTaskPhase(ctx, db.SetTaskPhaseParams{ID: taskID, Phase: next.Name}); err != nil {
				return err
			}
			if err := q.UpsertGate(ctx, db.UpsertGateParams{TaskID: taskID, Phase: next.Name}); err != nil {
				return err
			}
			if err := s.record(ctx, q, taskID, &task.Phase, next.Name, "advance", actor, reason, nil); err != nil {
				return err
			}
			if err := s.emit(ctx, q, "task.phase_changed", taskID, map[string]any{"from": task.Phase, "to": next.Name, "role": next.Role, "owner": next.Owner}); err != nil {
				return err
			}
		}
		out, err = q.GetTask(ctx, taskID)
		return err
	})
	return out, err
}

// Close finishes a task wherever it stands, with a reason in the audit trail. This is for work
// the world has finished for us: a quality rule that has come good again, where waiting for a
// person to walk the task through its phases would only teach them to ignore such tasks.
// Everything a person or an agent does still goes through Advance and its gates.
func (s *Service) Close(ctx context.Context, taskID pgtype.UUID, actor Actor, reason string) (db.Task, error) {
	if reason == "" {
		return db.Task{}, errors.New("closing a task requires a reason")
	}
	var out db.Task
	err := s.tx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, taskID)
		if err != nil {
			return err
		}
		if task.ClosedAt.Valid {
			out = task
			return nil // already finished; saying so twice is not an error
		}
		if err := q.SetTaskPhase(ctx, db.SetTaskPhaseParams{ID: taskID, Phase: task.Phase, Close: true}); err != nil {
			return err
		}
		if err := s.record(ctx, q, taskID, &task.Phase, task.Phase, "close", actor, reason, nil); err != nil {
			return err
		}
		if err := s.emit(ctx, q, "task.closed", taskID, map[string]any{"phase": task.Phase, "reason": reason}); err != nil {
			return err
		}
		out, err = q.GetTask(ctx, taskID)
		return err
	})
	return out, err
}

// Rollback moves a task back to an earlier phase. A reason is mandatory. When the
// ceiling for the target phase is reached the task is blocked instead of moved.
func (s *Service) Rollback(ctx context.Context, taskID pgtype.UUID, to string, actor Actor, reason string) (db.Task, error) {
	if reason == "" {
		return db.Task{}, errors.New("rollback requires a reason")
	}
	var out db.Task
	err := s.tx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, taskID)
		if err != nil {
			return err
		}
		if task.ClosedAt.Valid {
			return ErrTaskClosed
		}
		m, err := s.machine(ctx, task.ProjectID, task.Kind)
		if err != nil {
			return err
		}
		n, err := q.CountRollbacksInto(ctx, db.CountRollbacksIntoParams{TaskID: taskID, ToPhase: to})
		if err != nil {
			return err
		}
		if err := m.CanRollback(task.Phase, to, int(n)); err != nil {
			if errors.Is(err, ErrRollbackCeiling) {
				msg := err.Error()
				if err := q.SetTaskBlocked(ctx, db.SetTaskBlockedParams{ID: taskID, BlockedReason: &msg}); err != nil {
					return err
				}
				if err := q.SetGateBlocked(ctx, db.SetGateBlockedParams{TaskID: taskID, Phase: task.Phase, BlockedReason: &msg, BlockedBy: ptr("automation")}); err != nil {
					return err
				}
				if err := s.record(ctx, q, taskID, &task.Phase, task.Phase, "auto_block", Actor{Kind: ActorSystem}, msg, map[string]any{"requested_rollback_to": to, "requested_by": actor.Kind, "reason": reason}); err != nil {
					return err
				}
				if err := s.emit(ctx, q, "gate.blocked", taskID, map[string]any{"phase": task.Phase, "reason": msg}); err != nil {
					return err
				}
				out, err = q.GetTask(ctx, taskID)
				return err
			}
			return err
		}
		observePhaseTime(ctx, task)
		if err := q.SetTaskPhase(ctx, db.SetTaskPhaseParams{ID: taskID, Phase: to}); err != nil {
			return err
		}
		if err := q.UpsertGate(ctx, db.UpsertGateParams{TaskID: taskID, Phase: to}); err != nil {
			return err
		}
		// A rolled-back phase must be re-approved: clear the earlier approval and checks on
		// the target gate, or the task would leave the inbox and advance on an old decision.
		if err := q.ClearGateApproval(ctx, db.ClearGateApprovalParams{TaskID: taskID, Phase: to}); err != nil {
			return err
		}
		if err := q.SetGateChecks(ctx, db.SetGateChecksParams{TaskID: taskID, Phase: to, Checks: []byte("[]")}); err != nil {
			return err
		}
		if err := s.record(ctx, q, taskID, &task.Phase, to, "rollback", actor, reason, nil); err != nil {
			return err
		}
		if err := s.emit(ctx, q, "task.phase_changed", taskID, map[string]any{"from": task.Phase, "to": to, "rollback": true, "reason": reason}); err != nil {
			return err
		}
		out, err = q.GetTask(ctx, taskID)
		return err
	})
	return out, err
}

// Handoff changes the owner of the current phase without changing the phase.
func (s *Service) Handoff(ctx context.Context, taskID pgtype.UUID, toKind ActorKind, toID pgtype.UUID, actor Actor, reason string) error {
	return s.tx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, taskID)
		if err != nil {
			return err
		}
		if task.ClosedAt.Valid {
			return ErrTaskClosed
		}
		kind := string(toKind)
		if err := q.SetTaskOwner(ctx, db.SetTaskOwnerParams{ID: taskID, OwnerKind: &kind, OwnerID: toID}); err != nil {
			return err
		}
		if err := s.record(ctx, q, taskID, &task.Phase, task.Phase, "handoff", actor, reason, map[string]any{"to_kind": toKind, "to_id": uuidString(toID)}); err != nil {
			return err
		}
		return s.emit(ctx, q, "task.handed_off", taskID, map[string]any{"phase": task.Phase, "to_kind": toKind, "to_id": uuidString(toID)})
	})
}

// SetCheck records an automated verdict on the current gate. A failing check blocks the
// task by automation; a later passing check with the same name unblocks it.
func (s *Service) SetCheck(ctx context.Context, taskID pgtype.UUID, c Check) error {
	return s.tx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, taskID)
		if err != nil {
			return err
		}
		gate, err := q.GetGate(ctx, db.GetGateParams{TaskID: taskID, Phase: task.Phase})
		if err != nil {
			return err
		}
		var checks []Check
		if len(gate.Checks) > 0 {
			if err := json.Unmarshal(gate.Checks, &checks); err != nil {
				return err
			}
		}
		replaced := false
		for i := range checks {
			if checks[i].Name == c.Name {
				checks[i] = c
				replaced = true
			}
		}
		if !replaced {
			checks = append(checks, c)
		}
		b, _ := json.Marshal(checks)
		if err := q.SetGateChecks(ctx, db.SetGateChecksParams{TaskID: taskID, Phase: task.Phase, Checks: b}); err != nil {
			return err
		}
		failing := ""
		for _, ch := range checks {
			if ch.Status == "fail" {
				failing = ch.Name + ": " + ch.Detail
				break
			}
		}
		switch {
		case failing != "" && task.BlockedReason == nil:
			if err := q.SetTaskBlocked(ctx, db.SetTaskBlockedParams{ID: taskID, BlockedReason: &failing}); err != nil {
				return err
			}
			if err := q.SetGateBlocked(ctx, db.SetGateBlockedParams{TaskID: taskID, Phase: task.Phase, BlockedReason: &failing, BlockedBy: ptr("automation")}); err != nil {
				return err
			}
			if err := s.record(ctx, q, taskID, &task.Phase, task.Phase, "auto_block", Actor{Kind: ActorPlugin}, failing, map[string]any{"check": c}); err != nil {
				return err
			}
			return s.emit(ctx, q, "gate.blocked", taskID, map[string]any{"phase": task.Phase, "reason": failing})
		case failing == "" && task.BlockedReason != nil && gate.BlockedBy != nil && *gate.BlockedBy == "automation":
			if err := q.SetTaskBlocked(ctx, db.SetTaskBlockedParams{ID: taskID}); err != nil {
				return err
			}
			if err := q.ClearGateBlocked(ctx, db.ClearGateBlockedParams{TaskID: taskID, Phase: task.Phase}); err != nil {
				return err
			}
			if err := s.record(ctx, q, taskID, &task.Phase, task.Phase, "unblock", Actor{Kind: ActorPlugin}, "checks pass", map[string]any{"check": c}); err != nil {
				return err
			}
			return s.emit(ctx, q, "gate.unblocked", taskID, map[string]any{"phase": task.Phase})
		}
		return s.emit(ctx, q, "gate.check_set", taskID, map[string]any{"phase": task.Phase, "check": c})
	})
}

// Approve records a human approval on the current gate. It does not advance; Advance does.
func (s *Service) Approve(ctx context.Context, taskID pgtype.UUID, userID pgtype.UUID) error {
	return s.tx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, taskID)
		if err != nil {
			return err
		}
		if err := q.SetGateHumanApproval(ctx, db.SetGateHumanApprovalParams{TaskID: taskID, Phase: task.Phase, HumanApprovedBy: userID}); err != nil {
			return err
		}
		// Approving a phase approves what it produced; a later edit flags requirements_changed.
		if _, err := q.ApprovePhaseArtifacts(ctx, db.ApprovePhaseArtifactsParams{TaskID: taskID, Phase: task.Phase, ApprovedBy: userID}); err != nil {
			return err
		}
		return s.emit(ctx, q, "gate.approved", taskID, map[string]any{"phase": task.Phase, "by": uuidString(userID)})
	})
}

// EvaluateGate reports whether the current phase can be left, without changing anything.
func (s *Service) EvaluateGate(ctx context.Context, taskID pgtype.UUID) error {
	q := db.New(s.pool)
	task, err := q.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.BlockedReason != nil {
		return fmt.Errorf("%w: %s", ErrTaskBlocked, *task.BlockedReason)
	}
	m, err := s.machine(ctx, task.ProjectID, task.Kind)
	if err != nil {
		return err
	}
	cur, err := m.Phase(task.Phase)
	if err != nil {
		return err
	}
	gate, err := q.GetGate(ctx, db.GetGateParams{TaskID: taskID, Phase: task.Phase})
	if err != nil {
		return err
	}
	return gateSatisfied(cur, gate)
}

// MarkRequirementsChanged flags a task whose earlier artifact was edited after approval.
func (s *Service) MarkRequirementsChanged(ctx context.Context, taskID pgtype.UUID, actor Actor, reason string) error {
	return s.tx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, taskID)
		if err != nil {
			return err
		}
		if err := q.SetTaskRequirementsChanged(ctx, db.SetTaskRequirementsChangedParams{ID: taskID, RequirementsChanged: true}); err != nil {
			return err
		}
		if err := s.record(ctx, q, taskID, &task.Phase, task.Phase, "auto_block", actor, "requirements changed: "+reason, nil); err != nil {
			return err
		}
		return s.emit(ctx, q, "task.requirements_changed", taskID, map[string]any{"phase": task.Phase, "reason": reason})
	})
}

func gateSatisfied(p Phase, g db.Gate) error {
	if g.BlockedReason != nil {
		return fmt.Errorf("%w: %s", ErrTaskBlocked, *g.BlockedReason)
	}
	var checks []Check
	if len(g.Checks) > 0 {
		_ = json.Unmarshal(g.Checks, &checks)
	}
	autoOK := true
	for _, c := range checks {
		if c.Status != "pass" {
			autoOK = false
		}
	}
	humanOK := g.HumanApprovedAt.Valid
	switch p.Gate {
	case GateHuman:
		if !humanOK {
			return fmt.Errorf("%w: phase %q needs human approval", ErrGateNotSatisfied, p.Name)
		}
	case GateAuto:
		if !autoOK {
			return fmt.Errorf("%w: phase %q has non-passing checks", ErrGateNotSatisfied, p.Name)
		}
	case GateBoth:
		if !autoOK || !humanOK {
			return fmt.Errorf("%w: phase %q needs passing checks and human approval", ErrGateNotSatisfied, p.Name)
		}
	}
	return nil
}

func (s *Service) tx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) record(ctx context.Context, q *db.Queries, taskID pgtype.UUID, from *string, to, kind string, actor Actor, reason string, evidence map[string]any) error {
	if m, err := telemetry.Instruments(); err == nil {
		attrs := metric.WithAttributes(telemetry.Attr("kind", kind), telemetry.Attr("to", to))
		m.PhaseTransitions.Add(ctx, 1, attrs)
		if kind == "auto_block" {
			m.GateBlocked.Add(ctx, 1, metric.WithAttributes(telemetry.Attr("phase", to)))
		}
	}
	ev := []byte("{}")
	if evidence != nil {
		ev, _ = json.Marshal(evidence)
	}
	var r *string
	if reason != "" {
		r = &reason
	}
	_, err := q.InsertPhaseTransition(ctx, db.InsertPhaseTransitionParams{
		TaskID: taskID, FromPhase: from, ToPhase: to, Kind: kind,
		ActorKind: string(actor.Kind), ActorID: actor.ID, Reason: r, Evidence: ev,
	})
	return err
}

func (s *Service) emit(ctx context.Context, q *db.Queries, typ string, taskID pgtype.UUID, payload map[string]any) error {
	b, _ := json.Marshal(payload)
	_, err := q.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: "task", AggregateID: taskID, Payload: b})
	return err
}

func ptr(s string) *string { return &s }

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	s, _ := u.Value()
	str, _ := s.(string)
	return str
}

func observePhaseTime(ctx context.Context, task db.Task) {
	m, err := telemetry.Instruments()
	if err != nil || !task.PhaseEnteredAt.Valid {
		return
	}
	secs := time.Since(task.PhaseEnteredAt.Time).Seconds()
	m.TimeInPhase.Record(ctx, secs, metric.WithAttributes(telemetry.Attr("kind", task.Kind), telemetry.Attr("phase", task.Phase)))
}
