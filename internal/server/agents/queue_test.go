package agents

import (
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

// dequeued hands the dispatcher the last message.dequeued event of msg, as the consumer would.
func (w *world) dequeued(msg db.Message) {
	w.t.Helper()
	var payload []byte
	if err := w.pool.QueryRow(w.ctx, "SELECT payload FROM events WHERE type = $1 AND payload->>'messageId' = $2 ORDER BY id DESC LIMIT 1",
		conversations.EventMessageDequeued, conversations.UUIDString(msg.ID)).Scan(&payload); err != nil {
		w.t.Fatal(err)
	}
	if err := w.mod.Dispatcher.Handle(w.ctx, db.EventsAfterRow{Type: conversations.EventMessageDequeued, AggregateID: msg.ConversationID, Payload: payload}); err != nil {
		w.t.Fatal(err)
	}
}

// While an agent answers, a message a person queues (or a command, which always waits) asks nobody
// and steers nothing; when the turn ends the first of the queue starts the next turn, the rest wait
// their turn. "Send now as a steer" takes a queued message out and steers the running turn.
func TestQueuedMessagesWaitForTheTurnToEnd(t *testing.T) {
	w := newWorld(t)
	rec := &steers{}
	w.mod.Dispatcher = NewDispatcher(w.q, Guards{}, w.mod.Starter, NewSteerer(w.q, rec, w.mod.Voice, nil))
	reviewer := w.agent("reviewer")
	ask := w.post(w.admin, w.general.ID, "@reviewer look at PR #12", reviewer)
	turn := w.turnOf(reviewer, ask)

	queue := func(body string, intent string) db.Message {
		t.Helper()
		posted, err := w.chat.Messages.Post(w.ctx, w.admin, turn.ConversationID, conversations.Draft{Body: body, Intent: intent})
		if err != nil {
			t.Fatal(err)
		}
		w.dispatch(posted.Message)
		return posted.Message
	}
	next := queue("then look at the tests", conversations.IntentQueued)
	cmd := queue("/summarize", "")
	steer := queue("only the checkout part", "")
	if *next.InputIntent != conversations.IntentQueued || *cmd.InputIntent != conversations.IntentQueued || *steer.InputIntent != conversations.IntentSteer {
		t.Fatalf("intents: %v %v %v", *next.InputIntent, *cmd.InputIntent, *steer.InputIntent)
	}
	if got := rec.got[turn.JobID.Bytes]; len(got) != 1 || !strings.Contains(got[0], "only the checkout part") {
		t.Fatalf("only the steer steers: %v", got)
	}
	if n := w.count("SELECT count(*) FROM agent_turns WHERE trigger_message_id = ANY($1)", []any{next.ID, cmd.ID}); n != 0 {
		t.Fatal("a queued message asked")
	}
	views, err := w.chat.Queue.List(w.ctx, w.admin, turn.ConversationID)
	if err != nil || len(views) != 2 || views[0].Message.ID != next.ID {
		t.Fatalf("queue: %+v %v", views, err)
	}
	// Somebody else does not send another's queued message.
	if _, err := w.chat.Queue.SendNow(w.ctx, w.person("member"), cmd.ID); err == nil {
		t.Fatal("somebody else sent a queued message")
	}

	// The turn ends: the first queued message starts the next one, and nothing else does.
	job, _ := w.q.GetJob(w.ctx, turn.JobID)
	w.finished(job, "done", "done", "")
	if err := (QueueRunner{pool: w.pool}).Handle(w.ctx, db.EventsAfterRow{Type: "job.finished", AggregateID: job.ID}); err != nil {
		t.Fatal(err)
	}
	moved, _ := w.q.GetMessage(w.ctx, next.ID)
	if *moved.InputIntent != conversations.IntentTurnStart {
		t.Fatalf("the first queued message: %v", *moved.InputIntent)
	}
	w.dequeued(next)
	second := w.turnOf(reviewer, moved)
	if second.ID == turn.ID {
		t.Fatal("no next turn")
	}
	if still, _ := w.q.GetMessage(w.ctx, cmd.ID); *still.InputIntent != conversations.IntentQueued {
		t.Fatal("the second queued message did not wait")
	}

	// Send now: the command steers the turn that runs.
	sent, err := w.chat.Queue.SendNow(w.ctx, w.admin, cmd.ID)
	if err != nil || *sent.InputIntent != conversations.IntentPromotedToSteer {
		t.Fatalf("send now: %+v %v", sent, err)
	}
	w.dequeued(cmd)
	if got := rec.got[second.JobID.Bytes]; len(got) != 1 || !strings.Contains(got[0], "/summarize") {
		t.Fatalf("the steer of the next turn: %v", got)
	}
	if _, err := w.chat.Queue.SendNow(w.ctx, w.admin, cmd.ID); err == nil {
		t.Fatal("a message left the queue twice")
	}
}
