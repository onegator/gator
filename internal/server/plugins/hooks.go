package plugins

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// onEvent forwards domain events to the plugins of the task's project.
func (h *Host) onEvent(ctx context.Context, e events.Event) {
	q := db.New(h.pool)
	switch e.Type {
	case "task.created", "task.phase_changed", "gate.approved":
		id, ok := parseUUID(e.AggregateID)
		if !ok {
			return
		}
		t, err := q.GetTask(ctx, id)
		if err != nil {
			return
		}
		var p struct {
			From, To, Reason, Phase string
			Rollback                bool
		}
		_ = json.Unmarshal(e.Payload, &p)
		h.each(h.forProject(t.ProjectID, ""), func(i *instance) {
			switch e.Type {
			case "task.phase_changed":
				if i.b.manifest.HasHook(plugin.MethodPhaseTransition) {
					h.callLogged(ctx, i, plugin.MethodPhaseTransition, plugin.PhaseTransitionParams{Task: taskRef(t), From: p.From, To: p.To, Rollback: p.Rollback, Reason: p.Reason})
				}
				h.evaluate(ctx, i, t.ID.String())
			case "task.created":
				h.evaluate(ctx, i, t.ID.String())
			case "gate.approved":
				if i.b.manifest.HasHook(plugin.MethodArtifactApproved) {
					h.callLogged(ctx, i, plugin.MethodArtifactApproved, plugin.ArtifactApprovedParams{Task: taskRef(t), Phase: p.Phase})
				}
			}
		})
	case "job.finished":
		id, ok := parseUUID(e.AggregateID)
		if !ok {
			return
		}
		j, err := q.GetJob(ctx, id)
		if err != nil {
			return
		}
		insts := h.forProject(j.ProjectID, plugin.MethodJobFinish)
		if len(insts) == 0 {
			return
		}
		t, err := q.GetTask(ctx, j.TaskID)
		if err != nil {
			return
		}
		h.each(insts, func(i *instance) {
			h.callLogged(ctx, i, plugin.MethodJobFinish, plugin.JobFinishParams{Task: taskRef(t), Job: jobRef(j), Receipt: j.Receipt})
		})
	}
}

// each runs fn for every instance at once, so one slow plugin does not hold up the others.
func (h *Host) each(insts []*instance, fn func(*instance)) {
	var wg sync.WaitGroup
	for _, i := range insts {
		wg.Add(1)
		go func(i *instance) { defer wg.Done(); fn(i) }(i)
	}
	wg.Wait()
}

func (h *Host) callLogged(ctx context.Context, i *instance, method string, params any) {
	if err := h.call(ctx, i, method, params, nil); err != nil && ctx.Err() == nil {
		i.log().Warn("plugin hook failed", "hook", method, "err", err)
	}
}

// ReconcileChecks re-asks the plugins about gates whose checks have sat at "pending" since
// before `before`. A sender delivers a webhook at most once: GitHub does not retry a delivery
// its proxy dropped, and three in a row were lost here to a 502 that never reached this
// process. Without this the gate keeps showing the last thing it happened to hear, which may
// be a check that finished hours ago. Returns how many tasks were re-evaluated.
// An invalid projectID means every project, which is what the periodic sweep passes.
func (h *Host) ReconcileChecks(ctx context.Context, before time.Time, max int32) (int, error) {
	return h.reconcile(ctx, pgtype.UUID{}, before, max)
}

// ReconcileProjectChecks refreshes one project's gates. Scoped so a sweep can be bounded, and
// so a test can reconcile its own project without reaching into everyone else's.
func (h *Host) ReconcileProjectChecks(ctx context.Context, projectID pgtype.UUID, before time.Time, max int32) (int, error) {
	return h.reconcile(ctx, projectID, before, max)
}

func (h *Host) reconcile(ctx context.Context, projectID pgtype.UUID, before time.Time, max int32) (int, error) {
	if max <= 0 {
		max = 50
	}
	rows, err := db.New(h.pool).ListTasksWithStalePendingChecks(ctx, db.ListTasksWithStalePendingChecksParams{
		ProjectID: projectID, Before: pgtype.Timestamptz{Time: before, Valid: true}, MaxRows: max})
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		h.each(h.forProject(row.Task.ProjectID, plugin.MethodGateEvaluate), func(i *instance) {
			h.evaluate(ctx, i, row.Task.ID.String())
		})
	}
	return len(rows), nil
}

// evaluate asks a plugin for its checks on the task's current gate and stores them.
func (h *Host) evaluate(ctx context.Context, i *instance, taskID string) {
	if !i.b.manifest.HasHook(plugin.MethodGateEvaluate) {
		return
	}
	id, _ := parseUUID(taskID)
	t, err := db.New(h.pool).GetTask(ctx, id)
	if err != nil || t.ClosedAt.Valid {
		return
	}
	var r plugin.GateEvaluateResult
	if err := h.call(ctx, i, plugin.MethodGateEvaluate, plugin.GateEvaluateParams{Task: taskRef(t), Phase: t.Phase}, &r); err != nil {
		if ctx.Err() == nil {
			i.log().Warn("gate evaluation failed", "task", taskID, "err", err)
		}
		return
	}
	for _, c := range r.Checks {
		if validCheck(c) != nil {
			i.log().Warn("plugin returned an invalid check", "check", c.Name, "status", c.Status)
			continue
		}
		if err := h.proc.SetCheck(ctx, t.ID, process.Check{Name: c.Name, Source: "plugin:" + i.b.name, Status: c.Status, Detail: c.Detail}); err != nil {
			i.log().Warn("store plugin check", "check", c.Name, "err", err)
		}
	}
}

func jobRef(j db.Job) plugin.JobRef {
	return plugin.JobRef{ID: uuidString(j.ID), Role: j.Role, Phase: j.Phase, Backend: j.Backend, Status: j.Status}
}

// PrepareJob asks the project's plugins for instructions before a job is leased. It
// implements runners.JobPreparer. Failures are logged and skipped: a plugin never blocks work.
func (h *Host) PrepareJob(ctx context.Context, j db.Job) []process.ContextDoc {
	insts := h.forProject(j.ProjectID, plugin.MethodJobPrepare)
	if len(insts) == 0 {
		return nil
	}
	t, err := db.New(h.pool).GetTask(ctx, j.TaskID)
	if err != nil {
		return nil
	}
	var docs []process.ContextDoc
	for _, i := range insts {
		var r plugin.JobPrepareResult
		if err := h.call(ctx, i, plugin.MethodJobPrepare, plugin.JobPrepareParams{Task: taskRef(t), Job: jobRef(j)}, &r); err != nil {
			i.log().Warn("jobPrepare failed", "job", uuidString(j.ID), "err", err)
			continue
		}
		if r.Instructions != "" {
			docs = append(docs, process.ContextDoc{Kind: "plugin", Phase: j.Phase, Title: "From the " + i.b.name + " plugin", Body: r.Instructions})
		}
	}
	return docs
}

// UITab and UIChip are a plugin's contributions to a task's screens.
type UITab struct {
	Plugin, Title, Markdown string
}

// UIChip is a short label on a task card.
type UIChip struct {
	Plugin, Text, Color, URL string
}

// RenderUI collects tabs and chips for a task from its project's plugins.
func (h *Host) RenderUI(ctx context.Context, t db.Task) ([]UITab, []UIChip) {
	var tabs []UITab
	var chips []UIChip
	for _, i := range h.forProject(t.ProjectID, plugin.MethodRenderUI) {
		var r plugin.RenderUIResult
		if err := h.call(ctx, i, plugin.MethodRenderUI, plugin.RenderUIParams{Task: taskRef(t)}, &r); err != nil {
			i.log().Warn("renderUI failed", "err", err)
			continue
		}
		for _, x := range r.Tabs {
			tabs = append(tabs, UITab{Plugin: i.b.name, Title: x.Title, Markdown: x.Markdown})
		}
		for _, x := range r.Chips {
			chips = append(chips, UIChip{Plugin: i.b.name, Text: x.Text, Color: x.Color, URL: x.URL})
		}
	}
	return tabs, chips
}
