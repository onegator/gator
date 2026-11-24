package agents

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

// quietPost is a person's message that no dispatcher has read yet.
func (w *world) quietPost(p auth.Principal, conv pgtype.UUID, body string, agents ...db.Agent) db.Message {
	w.t.Helper()
	posted, err := w.chat.Messages.Post(w.ctx, p, conv, conversations.Draft{Body: body, Mentions: mentioning(agents...)})
	if err != nil {
		w.t.Fatal(err)
	}
	return posted.Message
}

// orphan is the turn a start left behind when it died before queueing the job, claimed this long
// ago (0: never claimed).
func (w *world) orphan(a db.Agent, msg db.Message, claimedAgo time.Duration) db.AgentTurn {
	w.t.Helper()
	home, err := w.chat.Threads.Under(w.ctx, msg)
	if err != nil {
		w.t.Fatal(err)
	}
	turn, err := w.q.InsertTurn(w.ctx, db.InsertTurnParams{AgentID: a.ID, ProjectID: w.project, ConversationID: home.ID,
		TriggerMessageID: msg.ID, RequestedByKind: RequestedByUser, RequestedBy: w.admin.UserID, Tier: TierChat, Status: TurnQueued})
	if err != nil {
		w.t.Fatal(err)
	}
	if claimedAgo > 0 {
		if _, err := w.pool.Exec(w.ctx, "UPDATE agent_turns SET claimed_at = now() - $2::interval, created_at = now() - $2::interval WHERE id = $1",
			turn.ID, claimedAgo.String()); err != nil {
			w.t.Fatal(err)
		}
	}
	return turn
}

func (w *world) jobsOfTurn(turn pgtype.UUID) int {
	return w.count("SELECT count(*) FROM jobs WHERE turn_id = $1", turn)
}

// Two dispatchers reading the same mention at once (two servers, a redelivery) start one turn
// with one job and one answer.
func TestConcurrentStartsQueueOneJob(t *testing.T) {
	w := newWorld(t)
	reviewer := w.agent("reviewer")
	for round := range 5 {
		msg := w.quietPost(w.admin, w.general.ID, "@reviewer round "+string(rune('a'+round)), reviewer)
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { w.dispatch(msg) })
		}
		wg.Wait()
		turn := w.turnOf(reviewer, msg)
		if !turn.JobID.Valid || w.jobsOfTurn(turn.ID) != 1 {
			t.Fatalf("round %d: %d jobs, turn %+v", round, w.jobsOfTurn(turn.ID), turn)
		}
		if n := w.count("SELECT count(*) FROM messages WHERE conversation_id = $1 AND author_kind = 'agent'", turn.ConversationID); n != 1 {
			t.Fatalf("round %d: %d answers", round, n)
		}
	}
}

// A start that died after queueing its job but before writing it on the turn, or while holding
// the claim, is finished by the next delivery, which queues no second job.
func TestAStartThatDiedIsFinishedOnRedelivery(t *testing.T) {
	w := newWorld(t)
	reviewer := w.agent("reviewer")
	msg := w.post(w.admin, w.general.ID, "@reviewer look", reviewer)
	turn := w.turnOf(reviewer, msg)
	if _, err := w.pool.Exec(w.ctx, "UPDATE agent_turns SET job_id = NULL, claimed_at = NULL WHERE id = $1", turn.ID); err != nil {
		t.Fatal(err)
	}
	w.dispatch(msg)
	again := w.turnOf(reviewer, msg)
	if again.JobID != turn.JobID || w.jobsOfTurn(turn.ID) != 1 {
		t.Fatalf("the job it had is found again: %v vs %v, %d jobs", again.JobID, turn.JobID, w.jobsOfTurn(turn.ID))
	}

	// A claim held by a start that is still alive keeps others out; one that expired does not.
	planner := w.agent("planner")
	held := w.quietPost(w.admin, w.general.ID, "@planner plan", planner)
	orphan := w.orphan(planner, held, time.Second)
	w.dispatch(held)
	if w.jobsOfTurn(orphan.ID) != 0 {
		t.Fatal("a turn being started by someone else was started twice")
	}
	if _, err := w.pool.Exec(w.ctx, "UPDATE agent_turns SET claimed_at = now() - interval '10 minutes' WHERE id = $1", orphan.ID); err != nil {
		t.Fatal(err)
	}
	w.dispatch(held)
	if got := w.turnOf(planner, held); !got.JobID.Valid || w.jobsOfTurn(orphan.ID) != 1 || !got.ReplyMessageID.Valid {
		t.Fatalf("an expired claim is taken over: %+v", got)
	}
}

// Asked again while waiting without a job, and refused now, the turn ends refused and says so.
func TestARedeliveredRefusalEndsAWaitingTurn(t *testing.T) {
	w := newWorld(t)
	reviewer := w.agent("reviewer")
	msg := w.quietPost(w.admin, w.general.ID, "@reviewer look", reviewer)
	turn := w.orphan(reviewer, msg, 0)
	if _, err := w.pool.Exec(w.ctx, "UPDATE agents SET status = 'paused' WHERE id = $1", reviewer.ID); err != nil {
		t.Fatal(err)
	}
	w.dispatch(msg)
	got, _ := w.q.GetTurn(w.ctx, turn.ID)
	if got.Status != TurnRefused || got.Refusal == nil || *got.Refusal != RefusedPaused {
		t.Fatalf("turn: %+v", got)
	}
	said := w.settled(turn.ConversationID, func(m []db.Message) bool { return len(m) > 0 })
	if len(said) != 1 || !strings.Contains(said[0].Body, "is paused") {
		t.Fatalf("said: %+v", said)
	}
}

// A turn without a job answers nothing, so it does not swallow the next mention of its agent as a
// steer; a stop of it before any job is the turn's end, and of one being started, a conflict
// until its job is there, which is then stopped.
func TestAJoblessTurnNeitherSteersNorHangs(t *testing.T) {
	w := newWorld(t)
	reviewer := w.agent("reviewer")
	first := w.quietPost(w.admin, w.general.ID, "@reviewer look", reviewer)
	orphan := w.orphan(reviewer, first, 0)
	next := w.post(w.admin, orphan.ConversationID, "@reviewer are you there?", reviewer)
	if fresh := w.turnOf(reviewer, next); !fresh.JobID.Valid {
		t.Fatalf("the new mention is a new turn: %+v", fresh)
	}

	stopped, err := w.mod.Control.Stop(w.ctx, w.admin, orphan.ID, "")
	if err != nil || stopped.Status != TurnStopped {
		t.Fatalf("stopping a jobless turn: %+v %v", stopped, err)
	}
	// Being started by someone else: a conflict, not a stop the start then ignores.
	other := w.quietPost(w.admin, w.general.ID, "@reviewer again", reviewer)
	starting := w.orphan(reviewer, other, time.Second)
	if _, err := w.mod.Control.Stop(w.ctx, w.admin, starting.ID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("stopping a turn being started: %v", err)
	}
}

// The sweeper ends a turn no delivery could start, and says when an answer waits for a runner.
func TestTheSweeperEndsWhatHangs(t *testing.T) {
	w := newWorld(t)
	reviewer, planner := w.agent("reviewer"), w.agent("planner")
	stuck := w.orphan(reviewer, w.quietPost(w.admin, w.general.ID, "@reviewer look", reviewer), 10*time.Minute)
	waiting := w.turnOf(planner, w.post(w.admin, w.general.ID, "@planner plan", planner))
	if _, err := w.pool.Exec(w.ctx, "UPDATE jobs SET created_at = now() - interval '10 minutes' WHERE id = $1", waiting.JobID); err != nil {
		t.Fatal(err)
	}
	if err := w.mod.Sweeper(nil).Sweep(w.ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.q.GetTurn(w.ctx, stuck.ID); got.Status != TurnFailed {
		t.Fatalf("the stuck turn: %s", got.Status)
	}
	said := w.settled(stuck.ConversationID, func(m []db.Message) bool { return len(m) > 0 })
	if len(said) == 0 || !strings.Contains(said[len(said)-1].Body, "could not start") {
		t.Fatalf("said: %+v", said)
	}
	if msg, _ := w.q.GetMessage(w.ctx, waiting.ReplyMessageID); msg.Body != Waiting(planner) || msg.Status != conversations.StatusStreaming {
		t.Fatalf("the waiting answer: %q", msg.Body)
	}
}

// A consent card lost between the turn's start and its question is asked on redelivery; one a
// person already answered is not asked again.
func TestConsentIsAskedAgainUntilAnswered(t *testing.T) {
	w := newWorld(t)
	worker := w.agent("worker")
	msg := w.post(w.admin, w.general.ID, "@worker fix it", worker)
	turn := w.turnOf(worker, msg)
	key := "consent:" + uuidText(turn.ID)
	if _, err := w.pool.Exec(w.ctx, "DELETE FROM messages WHERE card_key = $1", key); err != nil {
		t.Fatal(err)
	}
	w.dispatch(msg)
	card, err := w.q.FindCard(w.ctx, db.FindCardParams{CardKey: &key, ConversationIds: []pgtype.UUID{turn.ConversationID}})
	if err != nil {
		t.Fatalf("the card is asked again: %v", err)
	}
	if _, err := w.mod.Consent.Allow(w.ctx, w.admin, turn.ID); err != nil {
		t.Fatal(err)
	}
	w.dispatch(msg)
	card, _ = w.q.GetMessage(w.ctx, card.ID)
	if cardData(t, card)["state"] != ConsentAllowed {
		t.Fatal("an answered card was asked again")
	}
}
