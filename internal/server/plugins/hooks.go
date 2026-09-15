package plugins

import (
	"context"
	"encoding/json"
	"sync"

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
