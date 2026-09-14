package process

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/metric"

	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/server/telemetry"
)

// ErrInvalidUsage rejects malformed usage reports.
var ErrInvalidUsage = errors.New("invalid usage")

// TokenCounts splits tokens the way providers bill them.
type TokenCounts struct {
	Input, Output, CacheRead, CacheWrite int64
}

// Total is the sum of every token class.
func (t TokenCounts) Total() int64 { return t.Input + t.Output + t.CacheRead + t.CacheWrite }

func (t *TokenCounts) addRecord(u db.UsageRecord) {
	t.Input += u.InputTokens
	t.Output += u.OutputTokens
	t.CacheRead += u.CacheReadTokens
	t.CacheWrite += u.CacheWriteTokens
}

func (t *TokenCounts) add(o TokenCounts) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
}

// PhaseMetrics is time and usage for one phase of one task. A phase entered twice
// (after a rollback) accumulates both visits.
type PhaseMetrics struct {
	Phase          string
	Owner          string
	Visits         int
	Seconds        float64 // wall time in the phase
	BlockedSeconds float64 // part of Seconds spent blocked
	AgentSeconds   float64 // time agents ran while the task was in this phase
	WorkSeconds    float64 // job wall time from start to finish (agent plus setup and git)
	QueueSeconds   float64 // jobs waiting for a runner
	WaitingSeconds float64 // the rest of Seconds: nobody working, the phase waits for a person
	Tokens         TokenCounts
	CostUSD        float64
	Jobs           int
}

// Task states, most urgent first.
const (
	StateClosed          = "closed"
	StateBlocked         = "blocked"
	StateAgentStalled    = "agent_stalled"
	StateAgentWorking    = "agent_working"
	StateQueued          = "queued"
	StateWaitingForHuman = "waiting_for_human"
)

// TaskState is what the task is doing right now and since when.
type TaskState struct {
	Kind  string
	Since time.Time
}

// TaskMetrics is the full time and token account of a task.
type TaskMetrics struct {
	TaskID         pgtype.UUID
	Closed         bool
	LeadSeconds    float64 // created → closed, or → now while open
	BlockedSeconds float64
	AgentSeconds   float64
	// Lead = WorkSeconds + QueueSeconds + BlockedSeconds + WaitingSeconds (waiting is clamped at
	// zero when parallel jobs overlap).
	WorkSeconds    float64
	QueueSeconds   float64
	WaitingSeconds float64
	State          TaskState
	Tokens         TokenCounts
	CostUSD        float64
	CostEstimated  bool
	Jobs           int
	Phases         []PhaseMetrics // in order of first visit
}

// ComputeMetrics derives time per phase from the transition log, splits it into agent work,
// queue, blocked and waiting-for-a-person using the task's jobs, and adds usage records.
// It is pure so it can be tested with synthetic timelines.
//
// Rules: create/advance/rollback start a phase segment; advance/rollback/close end it;
// auto_block starts a blocked interval inside the current phase, unblock or leaving the
// phase ends it. Open tasks are measured up to now.
func ComputeMetrics(task db.Task, trs []db.PhaseTransition, usage []db.UsageRecord, jobs []db.Job, owners map[string]Owner, now time.Time) TaskMetrics {
	var order []string
	byPhase := map[string]*PhaseMetrics{}
	get := func(p string) *PhaseMetrics {
		if m, ok := byPhase[p]; ok {
			return m
		}
		m := &PhaseMetrics{Phase: p, Owner: string(owners[p])}
		byPhase[p] = m
		order = append(order, p)
		return m
	}

	var cur string
	var start, enteredAt, unblockedAt time.Time
	var blockedAt *time.Time
	closeSegment := func(at time.Time) {
		if cur == "" {
			return
		}
		m := get(cur)
		m.Seconds += at.Sub(start).Seconds()
		if blockedAt != nil {
			m.BlockedSeconds += at.Sub(*blockedAt).Seconds()
			blockedAt = nil
		}
	}
	enter := func(p string, at time.Time) {
		cur, start, enteredAt = p, at, at
		get(p).Visits++
	}
	for _, tr := range trs {
		at := tr.CreatedAt.Time
		switch tr.Kind {
		case "create":
			enter(tr.ToPhase, at)
		case "advance", "rollback":
			closeSegment(at)
			enter(tr.ToPhase, at)
		case "close":
			closeSegment(at)
			cur = ""
		case "auto_block":
			if cur != "" && blockedAt == nil {
				t := at
				blockedAt = &t
			}
		case "unblock":
			if cur != "" && blockedAt != nil {
				get(cur).BlockedSeconds += at.Sub(*blockedAt).Seconds()
				blockedAt = nil
				unblockedAt = at
			}
		}
	}
	var openBlock *time.Time
	if cur != "" {
		if blockedAt != nil {
			t := *blockedAt
			openBlock = &t
		}
		closeSegment(now)
	}

	out := TaskMetrics{TaskID: task.ID, Closed: task.ClosedAt.Valid}
	end := now
	if task.ClosedAt.Valid {
		end = task.ClosedAt.Time
	}
	out.LeadSeconds = end.Sub(task.CreatedAt.Time).Seconds()

	seenJobs := map[[16]byte]bool{}
	for _, u := range usage {
		p := get(u.Phase)
		p.Tokens.addRecord(u)
		p.CostUSD += u.CostUsd
		p.AgentSeconds += float64(u.DurationMs) / 1000
		switch {
		case !u.JobID.Valid:
			p.Jobs++ // reported without a job: count each report as one unit of work
		case !seenJobs[u.JobID.Bytes]:
			seenJobs[u.JobID.Bytes] = true
			p.Jobs++
		}
		if u.CostEstimated && u.CostUsd > 0 {
			out.CostEstimated = true
		}
	}

	for _, j := range jobs {
		p := get(j.Phase)
		jEnd := end
		if j.FinishedAt.Valid && j.FinishedAt.Time.Before(jEnd) {
			jEnd = j.FinishedAt.Time
		}
		created := j.CreatedAt.Time
		if j.StartedAt.Valid {
			p.QueueSeconds += positive(j.StartedAt.Time.Sub(created))
			p.WorkSeconds += positive(jEnd.Sub(j.StartedAt.Time))
		} else {
			p.QueueSeconds += positive(jEnd.Sub(created))
		}
	}

	for _, name := range order {
		p := byPhase[name]
		p.WaitingSeconds = math.Max(0, p.Seconds-p.BlockedSeconds-p.WorkSeconds-p.QueueSeconds)
		out.BlockedSeconds += p.BlockedSeconds
		out.AgentSeconds += p.AgentSeconds
		out.WorkSeconds += p.WorkSeconds
		out.QueueSeconds += p.QueueSeconds
		out.Tokens.add(p.Tokens)
		out.CostUSD += p.CostUSD
		out.Jobs += p.Jobs
		out.Phases = append(out.Phases, *p)
	}
	out.WaitingSeconds = math.Max(0, out.LeadSeconds-out.WorkSeconds-out.QueueSeconds-out.BlockedSeconds)
	out.State = taskState(task, jobs, cur, enteredAt, unblockedAt, openBlock)
	return out
}

// taskState says what the task is doing now. Waiting for a person starts at the latest of:
// entering the phase, being unblocked, or the last job of this phase finishing.
func taskState(task db.Task, jobs []db.Job, cur string, enteredAt, unblockedAt time.Time, openBlock *time.Time) TaskState {
	if task.ClosedAt.Valid {
		return TaskState{StateClosed, task.ClosedAt.Time}
	}
	fallback := func(t time.Time) time.Time {
		if t.IsZero() {
			return task.CreatedAt.Time
		}
		return t
	}
	if task.BlockedReason != nil {
		since := enteredAt
		if openBlock != nil {
			since = *openBlock
		}
		return TaskState{StateBlocked, fallback(since)}
	}
	var working, stalled, queued time.Time
	var latestDone time.Time
	earliest := func(cur, t time.Time) time.Time {
		if cur.IsZero() || t.Before(cur) {
			return t
		}
		return cur
	}
	for _, j := range jobs {
		switch j.Status {
		case "leased", "running":
			t := j.CreatedAt.Time
			if j.StartedAt.Valid {
				t = j.StartedAt.Time
			}
			working = earliest(working, t)
		case "stalled":
			t := j.StartedAt.Time
			if j.LastEventAt.Valid {
				t = j.LastEventAt.Time
			}
			stalled = earliest(stalled, t)
		case "queued":
			queued = earliest(queued, j.CreatedAt.Time)
		default:
			if j.Phase == cur && j.FinishedAt.Valid && j.FinishedAt.Time.After(latestDone) {
				latestDone = j.FinishedAt.Time
			}
		}
	}
	switch {
	case !stalled.IsZero():
		return TaskState{StateAgentStalled, stalled}
	case !working.IsZero():
		return TaskState{StateAgentWorking, working}
	case !queued.IsZero():
		return TaskState{StateQueued, queued}
	}
	since := enteredAt
	if unblockedAt.After(since) {
		since = unblockedAt
	}
	if latestDone.After(since) {
		since = latestDone
	}
	return TaskState{StateWaitingForHuman, fallback(since)}
}

func positive(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	return d.Seconds()
}

// Metrics loads a task's transitions and usage and computes its account.
func (s *Service) Metrics(ctx context.Context, taskID pgtype.UUID, now time.Time) (TaskMetrics, error) {
	q := db.New(s.pool)
	t, err := q.GetTask(ctx, taskID)
	if err != nil {
		return TaskMetrics{}, err
	}
	trs, err := q.ListPhaseTransitions(ctx, taskID)
	if err != nil {
		return TaskMetrics{}, err
	}
	us, err := q.ListUsageByTask(ctx, taskID)
	if err != nil {
		return TaskMetrics{}, err
	}
	jobs, err := q.ListJobsByTask(ctx, taskID)
	if err != nil {
		return TaskMetrics{}, err
	}
	owners := map[string]Owner{}
	if m, err := s.machine(ctx, t.ProjectID, t.Kind); err == nil {
		for _, p := range m.template.Phases {
			owners[p.Name] = p.Owner
		}
	}
	return ComputeMetrics(t, trs, us, jobs, owners, now), nil
}

// UsageInput is one usage report.
type UsageInput struct {
	JobID                                                        pgtype.UUID
	Phase, Backend, Model                                        string
	InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int64
	CostUSD                                                      float64
	CostEstimated                                                bool
	DurationMS                                                   int64
	StartedAt, FinishedAt                                        pgtype.Timestamptz
	IdempotencyKey                                               string
}

// RecordUsage stores a usage report against a task. It returns false when the idempotency
// key was already used (nothing stored). Phase defaults to the task's current phase.
func (s *Service) RecordUsage(ctx context.Context, taskID pgtype.UUID, in UsageInput, actor Actor) (bool, error) {
	if in.InputTokens < 0 || in.OutputTokens < 0 || in.CacheReadTokens < 0 || in.CacheWriteTokens < 0 || in.CostUSD < 0 || in.DurationMS < 0 {
		return false, fmt.Errorf("%w: values must not be negative", ErrInvalidUsage)
	}
	if in.StartedAt.Valid && in.FinishedAt.Valid && in.FinishedAt.Time.Before(in.StartedAt.Time) {
		return false, fmt.Errorf("%w: finishedAt before startedAt", ErrInvalidUsage)
	}
	recorded := false
	err := s.tx(ctx, func(q *db.Queries) error {
		t, err := q.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		phase := in.Phase
		if phase == "" {
			phase = t.Phase
		}
		var key *string
		if in.IdempotencyKey != "" {
			k := in.IdempotencyKey
			key = &k
		}
		n, err := q.InsertUsage(ctx, db.InsertUsageParams{
			TaskID: taskID, ProjectID: t.ProjectID, Phase: phase, JobID: in.JobID,
			Source: usageSource(actor.Kind), ActorID: actor.ID, Backend: in.Backend, Model: in.Model,
			InputTokens: in.InputTokens, OutputTokens: in.OutputTokens,
			CacheReadTokens: in.CacheReadTokens, CacheWriteTokens: in.CacheWriteTokens,
			CostUsd: in.CostUSD, CostEstimated: in.CostEstimated, DurationMs: in.DurationMS,
			StartedAt: in.StartedAt, FinishedAt: in.FinishedAt, IdempotencyKey: key,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		recorded = true
		total := in.InputTokens + in.OutputTokens + in.CacheReadTokens + in.CacheWriteTokens
		return s.emit(ctx, q, "task.usage_recorded", taskID, map[string]any{
			"phase": phase, "backend": in.Backend, "model": in.Model, "tokens": total,
			"cost_usd": in.CostUSD, "duration_ms": in.DurationMS,
		})
	})
	if recorded {
		if m, err := telemetry.Instruments(); err == nil {
			attrs := func(kind string) metric.AddOption {
				return metric.WithAttributes(telemetry.Attr("type", kind), telemetry.Attr("backend", in.Backend))
			}
			m.AgentTokens.Add(ctx, in.InputTokens, attrs("input"))
			m.AgentTokens.Add(ctx, in.OutputTokens, attrs("output"))
			m.AgentTokens.Add(ctx, in.CacheReadTokens, attrs("cache_read"))
			m.AgentTokens.Add(ctx, in.CacheWriteTokens, attrs("cache_write"))
			m.TokenCost.Add(ctx, in.CostUSD, metric.WithAttributes(telemetry.Attr("backend", in.Backend)))
		}
	}
	return recorded, err
}

func usageSource(k ActorKind) string {
	switch k {
	case ActorRunner:
		return "runner"
	case ActorPlugin:
		return "plugin"
	case ActorUser:
		return "user"
	}
	return "system"
}

// KindMetrics aggregates tasks of one kind in a project.
type KindMetrics struct {
	Kind           string
	Tasks          int64
	ClosedTasks    int64
	AvgLeadSeconds float64 // over closed tasks
	AgentSeconds   float64
	Tokens         TokenCounts
	CostUSD        float64
	Jobs           int64
}

// ProjectMetrics aggregates per kind for tasks created since `since`, plus a total row.
func (s *Service) ProjectMetrics(ctx context.Context, projectID pgtype.UUID, since time.Time) ([]KindMetrics, KindMetrics, error) {
	rows, err := db.New(s.pool).ProjectMetricsByKind(ctx, db.ProjectMetricsByKindParams{
		ProjectID: projectID, Since: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return nil, KindMetrics{}, err
	}
	total := KindMetrics{Kind: "all"}
	var leadWeighted float64
	out := make([]KindMetrics, 0, len(rows))
	for _, r := range rows {
		k := KindMetrics{
			Kind: r.Kind, Tasks: r.Tasks, ClosedTasks: r.ClosedTasks, AvgLeadSeconds: r.AvgLeadSeconds,
			AgentSeconds: float64(r.AgentMs) / 1000, CostUSD: r.CostUsd, Jobs: r.Jobs,
			Tokens: TokenCounts{Input: r.InputTokens, Output: r.OutputTokens, CacheRead: r.CacheReadTokens, CacheWrite: r.CacheWriteTokens},
		}
		out = append(out, k)
		total.Tasks += k.Tasks
		total.ClosedTasks += k.ClosedTasks
		total.AgentSeconds += k.AgentSeconds
		total.Tokens.add(k.Tokens)
		total.CostUSD += k.CostUSD
		total.Jobs += k.Jobs
		leadWeighted += k.AvgLeadSeconds * float64(k.ClosedTasks)
	}
	if total.ClosedTasks > 0 {
		total.AvgLeadSeconds = leadWeighted / float64(total.ClosedTasks)
	}
	return out, total, nil
}
