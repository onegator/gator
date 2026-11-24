package agents

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

// output is what a runner reports a job said, as session.events stores it.
func (w *world) output(job db.Job, seq int64, typ string, payload map[string]any) {
	w.t.Helper()
	b, _ := json.Marshal(payload)
	if _, err := w.q.InsertJobEvent(w.ctx, db.InsertJobEventParams{JobID: job.ID, Seq: seq, Type: typ, Payload: b,
		At: pgtype.Timestamptz{Time: time.Now(), Valid: true}}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) finished(job db.Job, status, summary, reason string) {
	w.t.Helper()
	receipt, _ := json.Marshal(map[string]any{"summary": summary})
	if _, err := w.pool.Exec(w.ctx, "UPDATE jobs SET status = $2, receipt = $3, stop_reason = NULLIF($4, '') WHERE id = $1", job.ID, status, receipt, reason); err != nil {
		w.t.Fatal(err)
	}
	for _, typ := range []string{"job.started", "job.finished"} {
		if err := w.mod.Tracker.Handle(w.ctx, db.EventsAfterRow{Type: typ, AggregateID: job.ID}); err != nil {
			w.t.Fatal(err)
		}
	}
}

// An answer is written into the agent's message as the agent writes it — its words, never its
// digest — while its tool calls move the job's card; at the end the message is the answer.
func TestAnAnswerIsStreamedIntoItsMessage(t *testing.T) {
	w := newWorld(t)
	reviewer := w.agent("reviewer")
	ask := w.post(w.admin, w.general.ID, "@reviewer look at PR #12", reviewer)
	turn := w.turnOf(reviewer, ask)
	job, _ := w.q.GetJob(w.ctx, turn.JobID)
	s := NewStreamer(nil, w.q, w.mod.Voice, w.pool, time.Millisecond, nil)

	w.output(job, 1, "text", map[string]any{"text": "Reading the diff."})
	w.output(job, 2, "tool_call", map[string]any{"name": "Read", "input": map[string]any{"file_path": "checkout.go"}})
	s.mark(w.ctx, uuidText(job.ID))
	s.flushOne(w.ctx, uuidText(job.ID))
	msg, _ := w.q.GetMessage(w.ctx, turn.ReplyMessageID)
	if msg.Body != "Reading the diff." || msg.Status != conversations.StatusStreaming {
		t.Fatalf("while it writes: %q %s", msg.Body, msg.Status)
	}
	if w.count("SELECT count(*) FROM events WHERE type = 'job.progress' AND aggregate_id = $1", job.ID) != 1 {
		t.Fatal("a tool call moves the job's card")
	}
	progress, _ := w.q.JobProgress(w.ctx, job.ID)
	if progress.ToolCalls != 1 || progress.LastTool != "Read" {
		t.Fatalf("progress: %+v", progress)
	}

	w.output(job, 3, "text", map[string]any{"text": "It rounds twice.\n\n```gator-digest\n{\"changes\":[]}\n```"})
	s.mark(w.ctx, uuidText(job.ID))
	s.flushOne(w.ctx, uuidText(job.ID))
	msg, _ = w.q.GetMessage(w.ctx, turn.ReplyMessageID)
	if msg.Body != "Reading the diff.\n\nIt rounds twice." {
		t.Fatalf("the digest is no part of the answer: %q", msg.Body)
	}
	if w.count("SELECT count(*) FROM events WHERE type = 'job.progress' AND aggregate_id = $1", job.ID) != 1 {
		t.Fatal("no new tool, no new progress")
	}

	w.finished(job, "done", "PR #12 rounds totals twice; see checkout.go:40.", "")
	msg, _ = w.q.GetMessage(w.ctx, turn.ReplyMessageID)
	turn, _ = w.q.GetTurn(w.ctx, turn.ID)
	if msg.Body != "PR #12 rounds totals twice; see checkout.go:40." || msg.Status != conversations.StatusFinal || turn.Status != TurnDone || !turn.FinishedAt.Valid {
		t.Fatalf("at the end: %q %s, turn %s", msg.Body, msg.Status, turn.Status)
	}
	// A late flush cannot undo the end.
	w.output(job, 4, "text", map[string]any{"text": "late"})
	s.mark(w.ctx, uuidText(job.ID))
	s.flushOne(w.ctx, uuidText(job.ID))
	if again, _ := w.q.GetMessage(w.ctx, turn.ReplyMessageID); again.Body != msg.Body || again.Status != conversations.StatusFinal {
		t.Fatalf("an ended answer was rewritten: %q", again.Body)
	}
}

// A turn that does not finish says so: a failed one why, a stopped one with what it had said.
func TestATurnThatEndsBadlySaysSo(t *testing.T) {
	cases := []struct {
		status, reason, said string
		want                 string
		msgStatus, turn      string
	}{
		{"failed", "claude error_max_turns", "", "@planner could not answer: claude error_max_turns", conversations.StatusFailed, TurnFailed},
		{"stopped", "stopped: by Ada", "Half a plan.", "Half a plan.\n\n_@planner stopped (by Ada)._", conversations.StatusFinal, TurnStopped},
		{"stopped", "", "", "_@planner stopped._", conversations.StatusFinal, TurnStopped},
	}
	for _, tc := range cases {
		t.Run(tc.status+"/"+tc.reason, func(t *testing.T) {
			w := newWorld(t)
			planner := w.agent("planner")
			ask := w.post(w.admin, w.general.ID, "@planner plan it", planner)
			turn := w.turnOf(planner, ask)
			job, _ := w.q.GetJob(w.ctx, turn.JobID)
			if tc.said != "" {
				w.output(job, 1, "text", map[string]any{"text": tc.said})
				s := NewStreamer(nil, w.q, w.mod.Voice, w.pool, time.Millisecond, nil)
				s.mark(w.ctx, uuidText(job.ID))
				s.flushOne(w.ctx, uuidText(job.ID))
			}
			w.finished(job, tc.status, "", tc.reason)
			msg, _ := w.q.GetMessage(w.ctx, turn.ReplyMessageID)
			turn, _ = w.q.GetTurn(w.ctx, turn.ID)
			if msg.Body != tc.want || msg.Status != tc.msgStatus || turn.Status != tc.turn {
				t.Fatalf("%q %s, turn %s", msg.Body, msg.Status, turn.Status)
			}
			if strings.Contains(msg.Body, "is reading") {
				t.Fatal("the placeholder is no answer")
			}
		})
	}
}
