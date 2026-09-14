package process

import (
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func near(a, b float64) bool { return math.Abs(a-b) < 0.001 }

func TestComputeMetricsTimeline(t *testing.T) {
	t0 := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	at := func(min int) pgtype.Timestamptz { return ts(t0.Add(time.Duration(min) * time.Minute)) }
	tr := func(min int, kind, to string) db.PhaseTransition {
		return db.PhaseTransition{Kind: kind, ToPhase: to, CreatedAt: at(min)}
	}
	task := db.Task{CreatedAt: at(0)}
	trs := []db.PhaseTransition{
		tr(0, "create", "planning"),
		tr(10, "auto_block", "planning"),
		tr(20, "unblock", "planning"),
		tr(30, "advance", "implementation"),
		tr(50, "rollback", "planning"),
		tr(60, "advance", "implementation"),
		tr(70, "handoff", "implementation"), // does not affect time
	}
	jobA := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	jobB := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	usage := []db.UsageRecord{
		{Phase: "implementation", JobID: jobA, InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 5000, DurationMs: 60_000, CostUsd: 0.5, CostEstimated: true},
		{Phase: "implementation", JobID: jobA, InputTokens: 10, OutputTokens: 5, DurationMs: 1_000, CostUsd: 0.01, CostEstimated: true},
		{Phase: "planning", JobID: jobB, InputTokens: 100, OutputTokens: 50, DurationMs: 10_000, CostUsd: 0.1},
		{Phase: "planning", InputTokens: 1, DurationMs: 500}, // manual report without job
	}
	owners := map[string]Owner{"planning": OwnerRunner, "implementation": OwnerRunner}
	m := ComputeMetrics(task, trs, usage, nil, owners, t0.Add(90*time.Minute))

	if m.Closed || !near(m.LeadSeconds, 90*60) {
		t.Fatalf("lead: closed=%v %v", m.Closed, m.LeadSeconds)
	}
	if len(m.Phases) != 2 || m.Phases[0].Phase != "planning" || m.Phases[1].Phase != "implementation" {
		t.Fatalf("phase order: %+v", m.Phases)
	}
	pl, im := m.Phases[0], m.Phases[1]
	if pl.Visits != 2 || !near(pl.Seconds, 40*60) || !near(pl.BlockedSeconds, 10*60) {
		t.Fatalf("planning: visits=%d seconds=%v blocked=%v", pl.Visits, pl.Seconds, pl.BlockedSeconds)
	}
	if im.Visits != 2 || !near(im.Seconds, 50*60) || im.BlockedSeconds != 0 {
		t.Fatalf("implementation: visits=%d seconds=%v", im.Visits, im.Seconds)
	}
	if im.Jobs != 1 || pl.Jobs != 2 || m.Jobs != 3 {
		t.Fatalf("jobs: impl=%d plan=%d total=%d", im.Jobs, pl.Jobs, m.Jobs)
	}
	if m.Tokens.Total() != 1000+200+5000+10+5+100+50+1 || m.Tokens.CacheRead != 5000 {
		t.Fatalf("tokens: %+v", m.Tokens)
	}
	if !near(m.AgentSeconds, 71.5) || !near(m.CostUSD, 0.61) || !m.CostEstimated {
		t.Fatalf("agent=%v cost=%v estimated=%v", m.AgentSeconds, m.CostUSD, m.CostEstimated)
	}
	if !near(m.BlockedSeconds, 600) {
		t.Fatalf("blocked total %v", m.BlockedSeconds)
	}
}

func TestComputeMetricsClosedTaskStopsClock(t *testing.T) {
	t0 := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	task := db.Task{CreatedAt: ts(t0), ClosedAt: ts(t0.Add(time.Hour))}
	trs := []db.PhaseTransition{
		{Kind: "create", ToPhase: "implementation", CreatedAt: ts(t0)},
		{Kind: "advance", ToPhase: "approved", CreatedAt: ts(t0.Add(40 * time.Minute))},
		{Kind: "auto_block", ToPhase: "approved", CreatedAt: ts(t0.Add(45 * time.Minute))},
		{Kind: "close", ToPhase: "approved", CreatedAt: ts(t0.Add(time.Hour))},
	}
	m := ComputeMetrics(task, trs, nil, nil, nil, t0.Add(24*time.Hour))
	if !m.Closed || !near(m.LeadSeconds, 3600) {
		t.Fatalf("lead %v", m.LeadSeconds)
	}
	if !near(m.Phases[1].Seconds, 20*60) || !near(m.Phases[1].BlockedSeconds, 15*60) {
		t.Fatalf("approved: %+v", m.Phases[1])
	}
	if m.Tokens.Total() != 0 || m.CostEstimated {
		t.Fatalf("no usage expected: %+v", m)
	}
}

func TestLeadSplitsIntoWorkQueueBlockedAndWaiting(t *testing.T) {
	t0 := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	at := func(min int) pgtype.Timestamptz { return ts(t0.Add(time.Duration(min) * time.Minute)) }
	task := db.Task{CreatedAt: at(0), Phase: "implementation"}
	trs := []db.PhaseTransition{{Kind: "create", ToPhase: "implementation", CreatedAt: at(0)}}
	job := db.Job{Phase: "implementation", Status: "done", CreatedAt: at(1), StartedAt: at(2), FinishedAt: at(5)}
	m := ComputeMetrics(task, trs, nil, []db.Job{job}, nil, t0.Add(15*time.Minute))

	if !near(m.LeadSeconds, 900) || !near(m.QueueSeconds, 60) || !near(m.WorkSeconds, 180) || !near(m.WaitingSeconds, 660) || m.BlockedSeconds != 0 {
		t.Fatalf("split: lead=%v queue=%v work=%v waiting=%v", m.LeadSeconds, m.QueueSeconds, m.WorkSeconds, m.WaitingSeconds)
	}
	if !near(m.WorkSeconds+m.QueueSeconds+m.BlockedSeconds+m.WaitingSeconds, m.LeadSeconds) {
		t.Fatal("parts must add up to the lead time")
	}
	p := m.Phases[0]
	if !near(p.WaitingSeconds, 660) || !near(p.WorkSeconds, 180) || !near(p.QueueSeconds, 60) {
		t.Fatalf("phase split: %+v", p)
	}
	// the job finished at minute 5, so the person has been the bottleneck since then
	if m.State.Kind != StateWaitingForHuman || !m.State.Since.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("state: %+v", m.State)
	}
}

func TestTaskStates(t *testing.T) {
	t0 := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	at := func(min int) pgtype.Timestamptz { return ts(t0.Add(time.Duration(min) * time.Minute)) }
	base := db.Task{CreatedAt: at(0), Phase: "implementation"}
	trs := []db.PhaseTransition{{Kind: "create", ToPhase: "implementation", CreatedAt: at(0)}}
	now := t0.Add(30 * time.Minute)
	reason := "ci red"

	cases := []struct {
		name  string
		task  db.Task
		trs   []db.PhaseTransition
		jobs  []db.Job
		kind  string
		since int
	}{
		{"fresh task waits since it entered the phase", base, trs, nil, StateWaitingForHuman, 0},
		{"queued job", base, trs, []db.Job{{Phase: "implementation", Status: "queued", CreatedAt: at(3)}}, StateQueued, 3},
		{"running job", base, trs, []db.Job{{Phase: "implementation", Status: "running", CreatedAt: at(3), StartedAt: at(4)}}, StateAgentWorking, 4},
		{"stalled job wins over a running one", base, trs, []db.Job{
			{Phase: "implementation", Status: "running", CreatedAt: at(3), StartedAt: at(4)},
			{Phase: "implementation", Status: "stalled", CreatedAt: at(5), StartedAt: at(6), LastEventAt: at(7)}}, StateAgentStalled, 7},
		{"blocked since the block", func() db.Task { t := base; t.BlockedReason = &reason; return t }(),
			append(append([]db.PhaseTransition{}, trs...), db.PhaseTransition{Kind: "auto_block", ToPhase: "implementation", CreatedAt: at(12)}), nil, StateBlocked, 12},
		{"unblock restarts the wait", base,
			append(append([]db.PhaseTransition{}, trs...),
				db.PhaseTransition{Kind: "auto_block", ToPhase: "implementation", CreatedAt: at(12)},
				db.PhaseTransition{Kind: "unblock", ToPhase: "implementation", CreatedAt: at(20)}), nil, StateWaitingForHuman, 20},
		{"closed", func() db.Task { t := base; t.ClosedAt = at(25); return t }(), trs, nil, StateClosed, 25},
	}
	for _, c := range cases {
		m := ComputeMetrics(c.task, c.trs, nil, c.jobs, nil, now)
		if m.State.Kind != c.kind || !m.State.Since.Equal(t0.Add(time.Duration(c.since)*time.Minute)) {
			t.Errorf("%s: got %s since %s", c.name, m.State.Kind, m.State.Since.Sub(t0))
		}
	}
}
