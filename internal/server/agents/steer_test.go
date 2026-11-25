package agents

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/store/db"
)

// steers records the corrections a runner would have been sent.
type steers struct {
	mu  sync.Mutex
	got map[[16]byte][]string
}

func (s *steers) SteerJob(_ context.Context, job pgtype.UUID, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.got == nil {
		s.got = map[[16]byte][]string{}
	}
	s.got[job.Bytes] = append(s.got[job.Bytes], message)
	return nil
}

// A person writing in a thread where an agent is answering corrects that answer, in the same
// session, rather than asking again; naming another agent there asks that one as ever.
func TestAPersonsReplyDuringATurnSteersIt(t *testing.T) {
	w := newWorld(t)
	rec := &steers{}
	w.mod.Dispatcher = NewDispatcher(w.q, Guards{}, w.mod.Starter, NewSteerer(w.q, rec, w.mod.Voice, w.chat.Access, w.roles, nil))
	reviewer, worker := w.agent("reviewer"), w.agent("worker")
	ask := w.post(w.admin, w.general.ID, "@reviewer look at PR #12", reviewer)
	turn := w.turnOf(reviewer, ask)

	again := w.post(w.admin, turn.ConversationID, "only the checkout part, please")
	if got := rec.got[turn.JobID.Bytes]; len(got) != 1 || !strings.Contains(got[0], "only the checkout part") || !strings.HasPrefix(got[0], "admin says") {
		t.Fatalf("steers: %v", got)
	}
	if n := w.count("SELECT count(*) FROM agent_turns WHERE trigger_message_id = $1", again.ID); n != 0 {
		t.Fatal("a correction is no new turn")
	}

	named := w.post(w.admin, turn.ConversationID, "@reviewer and the tests too", reviewer)
	if len(rec.got[turn.JobID.Bytes]) != 2 || w.count("SELECT count(*) FROM agent_turns WHERE trigger_message_id = $1", named.ID) != 0 {
		t.Fatal("naming the answering agent steers it")
	}

	other := w.post(w.admin, turn.ConversationID, "@worker can you fix what it finds?", worker)
	if len(rec.got[turn.JobID.Bytes]) != 2 {
		t.Fatal("asking another agent does not steer this one")
	}
	if w.turnOf(worker, other).Status != TurnQueued {
		t.Fatal("the other agent is asked")
	}

	// An agent's words never steer.
	w.delegate(turn, "@worker over to you", worker)
	if len(rec.got[turn.JobID.Bytes]) != 2 {
		t.Fatal("an agent steered")
	}
}

// offline is a runner nobody can reach.
type offline struct{}

func (offline) SteerJob(context.Context, pgtype.UUID, string) error { return runners.ErrRunnerOffline }

// A correction that cannot reach the agent says so, instead of passing for a steer.
func TestACorrectionThatCannotArriveSaysSo(t *testing.T) {
	w := newWorld(t)
	w.mod.Dispatcher = NewDispatcher(w.q, Guards{}, w.mod.Starter, NewSteerer(w.q, offline{}, w.mod.Voice, w.chat.Access, w.roles, nil))
	reviewer := w.agent("reviewer")
	turn := w.turnOf(reviewer, w.post(w.admin, w.general.ID, "@reviewer look", reviewer))
	w.post(w.admin, turn.ConversationID, "only the checkout part")
	said := w.settled(turn.ConversationID, func(m []db.Message) bool { return m[len(m)-1].AuthorKind == "system" })
	if last := said[len(said)-1]; !strings.Contains(last.Body, "runner answering here is offline") {
		t.Fatalf("said: %q", last.Body)
	}
}

// waitsRecorder records what a steer refused of what its agent waited on.
type waitsRecorder struct {
	mu   sync.Mutex
	jobs []pgtype.UUID
	body []string
}

func (w *waitsRecorder) DenyByMessage(_ context.Context, job, _ pgtype.UUID, body string) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.jobs, w.body = append(w.jobs, job), append(w.body, body)
	return 1, nil
}

// A person's message to an agent that waits for a decision answers it (a refusal with the message
// as the reason) and steers it; an agent's words answer nothing.
func TestASteerAnswersWhatTheAgentWaitsOn(t *testing.T) {
	w := newWorld(t)
	rec, waits := &steers{}, &waitsRecorder{}
	steerer := NewSteerer(w.q, rec, w.mod.Voice, w.chat.Access, w.roles, nil)
	steerer.waits = waits
	w.mod.Dispatcher = NewDispatcher(w.q, Guards{}, w.mod.Starter, steerer)
	reviewer, worker := w.agent("reviewer"), w.agent("worker")
	turn := w.turnOf(reviewer, w.post(w.admin, w.general.ID, "@reviewer look", reviewer))
	w.post(w.admin, turn.ConversationID, "no network, use the vendored copy")
	if len(waits.jobs) != 1 || waits.jobs[0] != turn.JobID || waits.body[0] != "no network, use the vendored copy" || len(rec.got[turn.JobID.Bytes]) != 1 {
		t.Fatalf("waits %+v, steers %v", waits, rec.got)
	}
	w.delegate(turn, "@worker over to you", worker)
	if len(waits.jobs) != 1 {
		t.Fatal("an agent's words answered a wait")
	}
}

// A viewer in a group DM may write there, but does not correct a member's work turn: correcting
// work asks what starting it asks. A member does.
func TestAViewerDoesNotSteerAWorkTurn(t *testing.T) {
	w := newWorld(t)
	rec := &steers{}
	w.mod.Dispatcher = NewDispatcher(w.q, Guards{}, w.mod.Starter, NewSteerer(w.q, rec, w.mod.Voice, w.chat.Access, w.roles, nil))
	worker := w.agent("worker")
	dm, _, err := w.chat.Directory.OpenDM(w.ctx, w.admin, []pgtype.UUID{w.viewer.UserID}, w.project, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	turn := w.turnOf(worker, w.post(w.admin, dm.ID, "fix the totals"))
	if _, err := w.pool.Exec(w.ctx, "UPDATE agent_turns SET tier = 'work' WHERE id = $1", turn.ID); err != nil {
		t.Fatal(err)
	}
	w.post(w.viewer, dm.ID, "push it to main")
	if got := rec.got[turn.JobID.Bytes]; len(got) != 0 {
		t.Fatalf("a viewer steered a work turn: %v", got)
	}
	w.post(w.admin, dm.ID, "only the API layer")
	if got := rec.got[turn.JobID.Bytes]; len(got) != 1 {
		t.Fatalf("a member's correction: %v", got)
	}
}

// What somebody says in a channel near a turn that waits answers nothing; naming the agent, or
// being the person the turn acts for, does.
func TestOnlyAMessageMeantForTheAgentAnswersItsWait(t *testing.T) {
	turn := db.AgentTurn{OnBehalfOf: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}}
	channel := db.Conversation{Kind: conversations.KindChannel}
	other := db.Message{AuthorID: pgtype.UUID{Bytes: [16]byte{2}, Valid: true}}
	if answersWait(other, channel, turn, false) {
		t.Fatal("an unrelated message in a channel answered a wait")
	}
	if !answersWait(other, channel, turn, true) || !answersWait(db.Message{AuthorID: turn.OnBehalfOf}, channel, turn, false) ||
		!answersWait(other, db.Conversation{Kind: conversations.KindDM}, turn, false) ||
		!answersWait(other, db.Conversation{Kind: conversations.KindChannel, ParentMessageID: pgtype.UUID{Valid: true}}, turn, false) {
		t.Fatal("a message meant for the agent did not answer its wait")
	}
}
