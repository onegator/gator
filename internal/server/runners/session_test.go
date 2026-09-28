package runners

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// fixture is one runner session, without its WebSocket: what the session does to the
// database when a runner reports is the part worth pinning down.
type fixture struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	m       *Manager
	s       *session
	tokens  *fakeTokens
	project pgtype.UUID
	backend string
}

type fakeTokens struct{ revoked []pgtype.UUID }

func (f *fakeTokens) IssueForJob(context.Context, pgtype.UUID, pgtype.UUID, time.Duration) (string, error) {
	return "agent-token", nil
}

func (f *fakeTokens) RevokeForJob(_ context.Context, jobID pgtype.UUID) error {
	f.revoked = append(f.revoked, jobID)
	return nil
}

func suffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("GATOR_DATABASE_URL")
	if url == "" {
		t.Skip("GATOR_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := db.New(pool)
	cat, err := process.DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	svc := process.NewService(pool, process.StaticCatalog(cat), process.StaticCapabilities(nil))
	tokens := &fakeTokens{}
	m := New(pool, svc, events.NewHub(), slog.New(slog.DiscardHandler), Config{AgentTokens: tokens})

	p, err := q.CreateProject(ctx, db.CreateProjectParams{Slug: "runners-" + suffix(), Name: "runners", Tags: []string{}, ProcessConfig: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM projects WHERE id=$1", p.ID) })

	f := &fixture{pool: pool, q: q, m: m, tokens: tokens, project: p.ID, backend: "test-" + suffix()}
	f.s = f.session(t)
	return f
}

// session registers a runner for this fixture's backend and project.
func (f *fixture) session(t *testing.T) *session {
	t.Helper()
	ctx := context.Background()
	name := "runner-" + suffix()
	_, rec, err := auth.Tokens{Pool: f.pool}.Issue(ctx, auth.IssueParams{Kind: auth.KindRunner, Scope: "runner:" + name, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	tok := rec.ID
	caps := proto.Capabilities{Backends: []string{f.backend}, MaxParallel: 2}
	capsJSON, _ := json.Marshal(caps)
	row, err := f.q.UpsertRunner(ctx, db.UpsertRunnerParams{TokenID: tok, Name: name, Location: "other", Capabilities: capsJSON, ProtocolVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	return &session{m: f.m, ctx: sctx, cancel: cancel, runnerID: row.ID, tokenID: tok, name: name, caps: caps, projects: []pgtype.UUID{f.project}}
}

// queuedJob queues a job in the fixture's project, for the fixture's backend.
func (f *fixture) queuedJob(t *testing.T) db.Job {
	t.Helper()
	ctx := context.Background()
	task, err := f.m.process.Create(ctx, process.CreateParams{ProjectID: f.project, Kind: "bug", Title: "Crash " + suffix()}, process.Actor{Kind: process.ActorSystem})
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.m.CreateJob(ctx, task.ID, NewJob{Role: "implementer", Backend: f.backend})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// leasedJob queues a job and leases it to s.
func (f *fixture) leasedJob(t *testing.T, s *session) db.Job {
	t.Helper()
	ctx := context.Background()
	job := f.queuedJob(t)
	leased, err := f.q.LeaseJobs(ctx, db.LeaseJobsParams{RunnerID: s.runnerID, LeaseUntil: ts(time.Now().Add(time.Minute)),
		Backends: []string{f.backend}, Projects: []pgtype.UUID{f.project}, Slots: 1})
	if err != nil || len(leased) != 1 || leased[0].ID != job.ID {
		t.Fatalf("lease: %v %v", leased, err)
	}
	return leased[0]
}

func (f *fixture) job(t *testing.T, id pgtype.UUID) db.Job {
	t.Helper()
	j, err := f.q.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// jobEvents lists the outbox event types about one job, oldest first.
func (f *fixture) jobEvents(t *testing.T, id pgtype.UUID) []string {
	t.Helper()
	return f.outbox(t, "job", id)
}

func (f *fixture) runnerEvents(t *testing.T, id pgtype.UUID) []string {
	t.Helper()
	return f.outbox(t, "runner", id)
}

func (f *fixture) outbox(t *testing.T, aggregate string, id pgtype.UUID) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), "SELECT type FROM events WHERE aggregate=$1 AND aggregate_id=$2 ORDER BY id", aggregate, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		out = append(out, typ)
	}
	return out
}

// roleArtifacts are the documents the job's role produced for its task.
func (f *fixture) roleArtifacts(t *testing.T, job db.Job) []db.Artifact {
	t.Helper()
	all, err := f.q.ListArtifacts(context.Background(), job.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	var out []db.Artifact
	for _, a := range all {
		if a.Type == process.ArtifactTypeFor(job.Role) {
			out = append(out, a)
		}
	}
	return out
}

func (f *fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var usage = proto.Usage{InputTokens: 100, OutputTokens: 20, DurationMS: 1500, CostUSD: 0.01}

func TestFinishDoneLeavesAnArtifactAndTheDigest(t *testing.T) {
	f := newFixture(t)
	job := f.leasedJob(t, f.s)
	fin := proto.Finish{JobID: uuidString(job.ID), Status: proto.StatusDone, Summary: "Fixed the crash.", Branch: "fix/crash",
		Commits: []string{"0123456789abcdef"}, ChangedFiles: 2, Usage: usage, Digest: &proto.Digest{Changes: []string{"guarded the nil"}}}
	if err := f.s.finish(fin); err != nil {
		t.Fatal(err)
	}
	got := f.job(t, job.ID)
	if got.Status != proto.StatusDone || got.StopReason != nil {
		t.Fatalf("status %q stop %v", got.Status, got.StopReason)
	}
	arts := f.roleArtifacts(t, job)
	if len(arts) != 1 {
		t.Fatalf("%d %s artifacts, want 1", len(arts), job.Role)
	}
	if c := *arts[0].Content; !strings.HasPrefix(c, "Fixed the crash.") || !strings.Contains(c, "`0123456789ab`") {
		t.Fatalf("artifact content %q", c)
	}
	if ev := f.jobEvents(t, job.ID); !slices.Contains(ev, "job.digest") || !slices.Contains(ev, "job.finished") || slices.Contains(ev, "job.digest_missing") {
		t.Fatalf("events %v", ev)
	}
	if f.count(t, "SELECT count(*) FROM usage_records WHERE job_id=$1", job.ID) != 1 {
		t.Fatal("usage was not recorded")
	}
	if !slices.Contains(f.tokens.revoked, job.ID) {
		t.Fatal("the agent's identity outlived its job")
	}
}

func TestFinishDoneWithoutDigestSaysSo(t *testing.T) {
	f := newFixture(t)
	job := f.leasedJob(t, f.s)
	if err := f.s.finish(proto.Finish{JobID: uuidString(job.ID), Status: proto.StatusDone, Usage: usage}); err != nil {
		t.Fatal(err)
	}
	if ev := f.jobEvents(t, job.ID); !slices.Contains(ev, "job.digest_missing") {
		t.Fatalf("events %v", ev)
	}
}

func TestFinishTurnsDoubtfulSuccessIntoFailure(t *testing.T) {
	cases := []struct {
		name   string
		fin    proto.Finish
		reason string
	}{
		{"unknown status", proto.Finish{Status: "splendid", Usage: usage}, `runner reported unknown status "splendid"`},
		{"done without usage", proto.Finish{Status: proto.StatusDone}, "done without a usage report"},
		{"usage that ends before it starts", proto.Finish{Status: proto.StatusDone, Usage: proto.Usage{InputTokens: 1,
			StartedAt: time.Now(), FinishedAt: time.Now().Add(-time.Hour)}}, "invalid usage report: "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.leasedJob(t, f.s)
			c.fin.JobID = uuidString(job.ID)
			if err := f.s.finish(c.fin); err != nil {
				t.Fatal(err)
			}
			got := f.job(t, job.ID)
			if got.Status != proto.StatusFailed || got.StopReason == nil || !strings.HasPrefix(*got.StopReason, c.reason) {
				t.Fatalf("status %q stop %v, want failed with %q", got.Status, got.StopReason, c.reason)
			}
			if len(f.roleArtifacts(t, job)) != 0 {
				t.Fatal("a failed job left an artifact")
			}
		})
	}
}

func TestFinishKeepsTheRunnersStopReason(t *testing.T) {
	f := newFixture(t)
	job := f.leasedJob(t, f.s)
	if err := f.s.finish(proto.Finish{JobID: uuidString(job.ID), Status: proto.StatusStopped, StopReason: "asked to stop"}); err != nil {
		t.Fatal(err)
	}
	if got := f.job(t, job.ID); got.Status != proto.StatusStopped || got.StopReason == nil || *got.StopReason != "asked to stop" {
		t.Fatalf("status %q stop %v", got.Status, got.StopReason)
	}
}

func TestFinishResentAfterReconnectIsANoOp(t *testing.T) {
	f := newFixture(t)
	job := f.leasedJob(t, f.s)
	fin := proto.Finish{JobID: uuidString(job.ID), Status: proto.StatusDone, Usage: usage}
	for range 2 {
		if err := f.s.finish(fin); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(t, "SELECT count(*) FROM receipts WHERE subject_id=$1", job.ID); n != 1 {
		t.Fatalf("%d receipts for one job", n)
	}
	if n := len(f.roleArtifacts(t, job)); n != 1 {
		t.Fatalf("%d artifacts for one job", n)
	}
}

func TestFinishRefusesWhatIsNotThisRunners(t *testing.T) {
	f := newFixture(t)
	job := f.leasedJob(t, f.s)
	other := f.session(t)
	for _, c := range []struct {
		s    *session
		id   string
		want string
	}{
		{other, uuidString(job.ID), "is not leased to this runner"},
		{f.s, "not-a-uuid", `bad job id "not-a-uuid"`},
		{f.s, "00000000-0000-0000-0000-000000000001", "unknown job"},
	} {
		err := c.s.finish(proto.Finish{JobID: c.id, Status: proto.StatusDone, Usage: usage})
		if err == nil || errors.Is(err, errRetry) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("finish %s: %v, want %q", c.id, err, c.want)
		}
	}
	if got := f.job(t, job.ID); got.Status != "leased" {
		t.Fatalf("someone else's finish moved the job to %q", got.Status)
	}
}

func TestEventsStartTheJobAndAreStoredOnce(t *testing.T) {
	f := newFixture(t)
	job := f.leasedJob(t, f.s)
	sub, unsub := f.m.hub.Subscribe("*")
	defer unsub()
	batch := proto.Events{JobID: uuidString(job.ID), Events: []proto.JobEvent{
		{Seq: 1, Type: "assistant", Payload: json.RawMessage(`{"text":"looking"}`)},
		{Seq: 2, Type: "tool_call"},
	}}
	for range 2 { // the second is a resend
		if err := f.s.events(batch); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.job(t, job.ID); got.Status != "running" || !got.StartedAt.Valid {
		t.Fatalf("status %q started %v", got.Status, got.StartedAt)
	}
	if n := f.count(t, "SELECT count(*) FROM job_events WHERE job_id=$1", job.ID); n != 2 {
		t.Fatalf("%d job events stored, want 2", n)
	}
	var payload string
	_ = f.pool.QueryRow(context.Background(), "SELECT payload::text FROM job_events WHERE job_id=$1 AND seq=2", job.ID).Scan(&payload)
	if payload != "{}" {
		t.Fatalf("empty payload stored as %q", payload)
	}
	if ev := f.jobEvents(t, job.ID); slices.Index(ev, "job.started") < 0 || strings.Count(strings.Join(ev, ","), "job.started") != 1 {
		t.Fatalf("events %v", ev)
	}
	published := 0
	for len(sub) > 0 {
		if e := <-sub; e.Type == "job.output" {
			published++
		}
	}
	if published != 2 {
		t.Fatalf("%d job.output published, want one per new event", published)
	}
}

func TestEventsResumeAStalledJob(t *testing.T) {
	f := newFixture(t)
	job := f.leasedJob(t, f.s)
	if _, err := f.pool.Exec(context.Background(), "UPDATE jobs SET status='stalled' WHERE id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.events(proto.Events{JobID: uuidString(job.ID), Events: []proto.JobEvent{{Seq: 1, Type: "assistant"}}}); err != nil {
		t.Fatal(err)
	}
	if ev := f.jobEvents(t, job.ID); !slices.Contains(ev, "job.resumed") || slices.Contains(ev, "job.started") {
		t.Fatalf("events %v", ev)
	}
}

func TestEventsRefuseWhatIsNotThisRunnersOrNotActive(t *testing.T) {
	f := newFixture(t)
	job := f.leasedJob(t, f.s)
	other := f.session(t)
	one := []proto.JobEvent{{Seq: 1, Type: "assistant"}}
	if err := other.events(proto.Events{JobID: uuidString(job.ID), Events: one}); err == nil || !strings.Contains(err.Error(), "not leased to this runner") {
		t.Fatalf("other runner: %v", err)
	}
	if err := f.s.finish(proto.Finish{JobID: uuidString(job.ID), Status: proto.StatusFailed, StopReason: "gave up"}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.events(proto.Events{JobID: uuidString(job.ID), Events: one}); err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("finished job: %v", err)
	}
}
