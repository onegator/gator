package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/brain/curator"
	"github.com/onegator/gator/internal/server/store/db"
)

// talkedThread is a task's thread with four messages in it, and the task.
func (h *harness) talkedThread() (gen.Task, gen.ConversationDetail) {
	h.t.Helper()
	task := h.task()
	var thread gen.ConversationDetail
	h.do("GET", "/tasks/"+task.Id.String()+"/conversation", nil, &thread)
	for i := range 4 {
		body := fmt.Sprintf("line %d", i)
		h.do("POST", "/conversations/"+thread.Id.String()+"/messages", gen.NewMessage{Body: &body}, nil)
	}
	return task, thread
}

// curationJob asks for a curation of thread now and answers its job.
func (h *harness) curationJob(thread gen.ConversationDetail) string {
	h.t.Helper()
	var id pgtype.UUID
	_ = id.Scan(thread.Id.String())
	conv, err := db.New(h.pool).GetConversation(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	if ok, err := h.curator.Curate(context.Background(), conv); err != nil || !ok {
		h.t.Fatalf("curate: %v %v", ok, err)
	}
	var job string
	if err := h.pool.QueryRow(context.Background(), `SELECT job_id::text FROM brain_curations WHERE conversation_id = $1`, thread.Id.String()).Scan(&job); err != nil {
		h.t.Fatal(err)
	}
	return job
}

// endCuration ends a curation job as status with the answer, and tells the curator, as the outbox
// would.
func (h *harness) endCuration(job, status, answer string) func() error {
	h.t.Helper()
	receipt, _ := json.Marshal(map[string]string{"summary": answer})
	if _, err := h.pool.Exec(context.Background(), `UPDATE jobs SET status = $2, receipt = $3 WHERE id = $1`, job, status, receipt); err != nil {
		h.t.Fatal(err)
	}
	var id pgtype.UUID
	_ = id.Scan(job)
	return func() error {
		return h.curator.Handle(context.Background(), db.EventsAfterRow{Type: "job.finished", AggregateID: id})
	}
}

func proposalsAnswer(titles ...string) string {
	var ps []map[string]any
	for _, t := range titles {
		ps = append(ps, map[string]any{"kind": "decision", "title": t, "content": "Because " + t + ".", "quote": t, "seq": 1})
	}
	b, _ := json.Marshal(map[string]any{"proposals": ps})
	return curator.Fence + "\n" + string(b) + "\n```"
}

// A curation's end is stored once however often it is delivered, leaves out what the Brain
// refuses rather than everything, and does not fill a thread past its cap.
func TestACurationEndsOnceAndWithinItsCap(t *testing.T) {
	h := newHarness(t)
	_, thread := h.talkedThread()
	proposed := func() int {
		return h.count(`SELECT count(*) FROM brain_entries WHERE source_conversation_id = $1`, thread.Id.String())
	}
	end := h.endCuration(h.curationJob(thread), "done", proposalsAnswer("Use Postgres", strings.Repeat("too long ", 50), "Use River"))
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if err := end(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := proposed(); n != 2 {
		t.Fatalf("two proposals, once each, the refused one left out: %d", n)
	}

	h.curator.MaxOpen = 3
	t.Cleanup(func() { h.curator.MaxOpen = curator.MaxOpenPerConversation })
	body := "and more"
	h.do("POST", "/conversations/"+thread.Id.String()+"/messages", gen.NewMessage{Body: &body}, nil)
	if err := h.endCuration(h.curationJob(thread), "done", proposalsAnswer("A", "B", "C"))(); err != nil {
		t.Fatal(err)
	}
	if n := proposed(); n != 3 {
		t.Fatalf("no more than the cap waits from one thread: %d", n)
	}
}

// A curation that failed is tried again later, later each time, not every minute; a task that
// closes with nothing said since the last curation is not read again.
func TestAFailedCurationBacksOffAndAClosedTaskWithNothingNewIsLeft(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	task, thread := h.talkedThread()
	if err := h.endCuration(h.curationJob(thread), "failed", "")(); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var next time.Time
	if err := h.pool.QueryRow(ctx, `SELECT attempts, next_at FROM brain_curations WHERE conversation_id = $1`, thread.Id.String()).Scan(&attempts, &next); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || time.Until(next) < 9*time.Minute {
		t.Fatalf("tried again in ten minutes: %d %v", attempts, next)
	}
	h.curator.Quiet = 0
	t.Cleanup(func() { h.curator.Quiet = curator.DefaultQuiet })
	h.curator.Sweep(ctx)
	jobs := func() int {
		return h.count(`SELECT count(*) FROM jobs WHERE task_id = $1 AND role = 'curator'`, task.Id.String())
	}
	if n := jobs(); n != 1 {
		t.Fatalf("not before then: %d curation jobs", n)
	}

	// Done, then the task closes with nothing new said.
	if _, err := h.pool.Exec(ctx, `UPDATE brain_curations SET next_at = NULL WHERE conversation_id = $1`, thread.Id.String()); err != nil {
		t.Fatal(err)
	}
	if err := h.endCuration(h.curationJob(thread), "done", proposalsAnswer())(); err != nil {
		t.Fatal(err)
	}
	var tid pgtype.UUID
	_ = tid.Scan(task.Id.String())
	if err := h.curator.Handle(ctx, db.EventsAfterRow{Type: "task.closed", AggregateID: tid}); err != nil {
		t.Fatal(err)
	}
	if n := jobs(); n != 2 {
		t.Fatalf("a task closing with nothing new said asks for no curation: %d", n)
	}
}

// What a thread settled is proposed within an hour of it going quiet, with the server's own
// settings: a thread quiet for half an hour is read by the next sweep, one that spoke ten minutes
// ago is not yet.
func TestAQuietThreadIsCuratedWithinTheHour(t *testing.T) {
	if curator.DefaultQuiet+curator.DefaultEvery >= curator.Promise {
		t.Fatal("the defaults cannot keep the promise")
	}
	h := newHarness(t)
	ctx := context.Background()
	_, quiet := h.talkedThread()
	_, recent := h.talkedThread()
	for id, ago := range map[string]time.Duration{quiet.Id.String(): curator.DefaultQuiet + time.Minute, recent.Id.String(): 10 * time.Minute} {
		if _, err := h.pool.Exec(ctx, `UPDATE conversations SET last_message_at = now() - $2::interval WHERE id = $1`, id, ago.String()); err != nil {
			t.Fatal(err)
		}
	}
	if h.curator.Quiet != curator.DefaultQuiet {
		t.Fatalf("the harness curates with the defaults: %v", h.curator.Quiet)
	}
	if _, err := h.curator.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	curated := func(thread string) bool {
		return h.count(`SELECT count(*) FROM brain_curations WHERE conversation_id = $1 AND job_id IS NOT NULL`, thread) == 1
	}
	if !curated(quiet.Id.String()) || curated(recent.Id.String()) {
		t.Fatalf("quiet for half an hour: %v; ten minutes: %v", curated(quiet.Id.String()), curated(recent.Id.String()))
	}
}
