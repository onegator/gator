package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/process"
)

func (s *Server) RecordTaskUsage(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.UsageInput
	if !decode(w, r, &in) {
		return
	}
	u := process.UsageInput{
		Phase: deref(in.Phase), Backend: deref(in.Backend), Model: deref(in.Model),
		InputTokens: i64(in.InputTokens), OutputTokens: i64(in.OutputTokens),
		CacheReadTokens: i64(in.CacheReadTokens), CacheWriteTokens: i64(in.CacheWriteTokens),
		DurationMS: in.DurationMs, IdempotencyKey: deref(in.IdempotencyKey),
		CostEstimated: true,
	}
	if in.JobId != nil {
		u.JobID = fromUUID(*in.JobId)
	}
	if in.CostUsd != nil {
		u.CostUSD = *in.CostUsd
	}
	if in.CostEstimated != nil {
		u.CostEstimated = *in.CostEstimated
	}
	if in.StartedAt != nil {
		u.StartedAt = pgtype.Timestamptz{Time: *in.StartedAt, Valid: true}
	}
	if in.FinishedAt != nil {
		u.FinishedAt = pgtype.Timestamptz{Time: *in.FinishedAt, Valid: true}
	}
	recorded, err := s.Process.RecordUsage(r.Context(), fromUUID(taskId), u, ActorFromContext(r.Context()))
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusCreated
	if !recorded {
		status = http.StatusOK
	}
	writeJSON(w, status, gen.UsageAck{Recorded: recorded})
}

func (s *Server) GetTaskMetrics(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleViewer); !ok {
		return
	}
	m, err := s.Process.Metrics(r.Context(), fromUUID(taskId), time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := gen.TaskMetrics{
		TaskId: toUUID(m.TaskID), Closed: m.Closed, LeadSeconds: m.LeadSeconds, BlockedSeconds: m.BlockedSeconds,
		AgentSeconds: m.AgentSeconds, Tokens: toTokens(m.Tokens), CostUsd: m.CostUSD, CostEstimated: m.CostEstimated,
		WorkSeconds: m.WorkSeconds, QueueSeconds: m.QueueSeconds, WaitingSeconds: m.WaitingSeconds,
		State: gen.TaskState{Kind: gen.TaskStateKind(m.State.Kind), Since: m.State.Since},
		Jobs:  m.Jobs, Phases: make([]gen.PhaseMetrics, 0, len(m.Phases)),
	}
	for _, p := range m.Phases {
		out.Phases = append(out.Phases, gen.PhaseMetrics{
			Phase: p.Phase, Owner: p.Owner, Visits: p.Visits, Seconds: p.Seconds, BlockedSeconds: p.BlockedSeconds,
			AgentSeconds: p.AgentSeconds, WorkSeconds: p.WorkSeconds, QueueSeconds: p.QueueSeconds, WaitingSeconds: p.WaitingSeconds,
			Tokens: toTokens(p.Tokens), CostUsd: p.CostUSD, Jobs: p.Jobs,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) GetProjectMetrics(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, params gen.GetProjectMetricsParams) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	since := time.Now().Add(-30 * 24 * time.Hour)
	if params.Since != nil {
		since = *params.Since
	}
	rows, total, err := s.Process.ProjectMetrics(r.Context(), fromUUID(projectId), since)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := gen.ProjectMetrics{ProjectId: projectId, Since: since, Totals: toKind(total), ByKind: make([]gen.KindMetrics, 0, len(rows))}
	for _, k := range rows {
		out.ByKind = append(out.ByKind, toKind(k))
	}
	writeJSON(w, http.StatusOK, out)
}

func toTokens(t process.TokenCounts) gen.Tokens {
	return gen.Tokens{Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite, Total: t.Total()}
}

func toKind(k process.KindMetrics) gen.KindMetrics {
	return gen.KindMetrics{Kind: k.Kind, Tasks: k.Tasks, ClosedTasks: k.ClosedTasks, AvgLeadSeconds: k.AvgLeadSeconds,
		AgentSeconds: k.AgentSeconds, Tokens: toTokens(k.Tokens), CostUsd: k.CostUSD, Jobs: k.Jobs}
}

func i64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
