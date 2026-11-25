package runners

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// A chat job goes only to a backend its runner says keeps it read-only, and a summary only to one
// that runs it with no tools: never to one that would refuse it.
func TestJobsGoOnlyWhereTheirLimitsHold(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	task, err := f.m.process.Create(ctx, process.CreateParams{ProjectID: f.project, Kind: "bug", Title: "Limits " + suffix()}, process.Actor{Kind: process.ActorSystem})
	if err != nil {
		t.Fatal(err)
	}
	f.allowAgents(t, task.ID)
	chat, err := f.m.CreateJob(ctx, task.ID, NewJob{Role: "reviewer", Backend: f.backend, Kind: KindReply, Tier: TierChat})
	if err != nil {
		t.Fatal(err)
	}
	lease := func(readOnly, noTools []string) []db.Job {
		t.Helper()
		jobs, err := f.q.LeaseJobs(ctx, db.LeaseJobsParams{RunnerID: f.s.runnerID, LeaseUntil: ts(time.Now().Add(time.Minute)),
			Backends: []string{f.backend}, Projects: []pgtype.UUID{f.project}, Slots: 10, Placement: chat.Placement,
			ReadOnly: readOnly, NoTools: noTools})
		if err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	if got := lease([]string{}, []string{f.backend}); len(got) != 0 {
		t.Fatalf("a chat job was leased to a backend that cannot keep it read-only: %+v", got)
	}
	if got := lease([]string{f.backend}, []string{}); len(got) != 1 || got[0].ID != chat.ID {
		t.Fatalf("lease %+v", got)
	}
	// A runner that says nothing keeps both with every backend, as before.
	if got := keeping([]string{"claude", "acp:gemini"}, nil); len(got) != 2 {
		t.Errorf("keeping %v", got)
	}
	if got := keeping([]string{"claude", "acp:gemini"}, []string{"claude"}); len(got) != 1 || got[0] != "claude" {
		t.Errorf("keeping %v", got)
	}
}

// A summary runs on the server's default backend whatever the project's policy picked: its role's
// backend may be an agent that cannot run without tools.
func TestASummaryRunsOnTheDefaultBackend(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE projects SET process_config = '{"policy":{"default":{"backend":"acp:codex"}}}' WHERE id = $1`, f.project); err != nil {
		t.Fatal(err)
	}
	task, err := f.m.process.Create(ctx, process.CreateParams{ProjectID: f.project, Kind: "bug", Title: "Summary " + suffix()}, process.Actor{Kind: process.ActorSystem})
	if err != nil {
		t.Fatal(err)
	}
	sum, err := f.m.CreateJob(ctx, task.ID, NewJob{Kind: KindSummary, Tier: TierChat, Role: "summarizer", Instruction: "sum up"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Backend != f.m.cfg.DefaultBackend || sum.Backend == "acp:codex" {
		t.Errorf("summary backend %q", sum.Backend)
	}
}
