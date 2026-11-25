package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Whether the Brain helps is counted: the median context of a phase job before and after a moment,
// and the share of phases sent back.
func TestBrainImpactIsMeasured(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	task := h.task()
	pivot := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	job := func(bytes int, at time.Time) {
		if _, err := h.pool.Exec(ctx, `INSERT INTO jobs (task_id, project_id, phase, role, backend, context_bytes, created_at)
			VALUES ($1, $2, 'planning', 'planner', 'claude', $3, $4)`, task.Id.String(), task.ProjectId.String(), bytes, at); err != nil {
			t.Fatal(err)
		}
	}
	move := func(kind string, at time.Time) {
		if _, err := h.pool.Exec(ctx, `INSERT INTO phase_transitions (task_id, from_phase, to_phase, kind, actor_kind, created_at)
			VALUES ($1, 'planning', 'implementation', $2, 'system', $3)`, task.Id.String(), kind, at); err != nil {
			t.Fatal(err)
		}
	}
	for i, b := range []int{1000, 1100, 1200} {
		job(b, pivot.Add(-time.Duration(i+1)*time.Hour))
	}
	for i, b := range []int{1150, 1250, 1300} {
		job(b, pivot.Add(time.Duration(i+1)*time.Hour))
	}
	for i, k := range []string{"advance", "rollback", "advance", "rollback"} {
		move(k, pivot.Add(-time.Duration(i+1)*time.Minute))
	}
	for i, k := range []string{"advance", "advance", "advance", "rollback"} {
		move(k, pivot.Add(time.Duration(i+1)*time.Minute))
	}
	var project pgtype.UUID
	_ = project.Scan(task.ProjectId.String())
	impact, err := h.quality.BrainImpact(ctx, project, pivot, 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if impact.Before.MedianContextBytes != 1100 || impact.After.MedianContextBytes != 1250 {
		t.Fatalf("medians: %+v", impact)
	}
	if impact.Before.RollbackRate != 0.5 || impact.After.RollbackRate != 0.25 {
		t.Fatalf("rollback rates: %+v", impact)
	}
	if !impact.ContextHeld || !impact.RollbacksFell {
		t.Fatalf("14%% more context and half the rollbacks is the Brain helping: %+v", impact)
	}
}
