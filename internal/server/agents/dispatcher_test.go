package agents

import (
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// "@reviewer look at PR #12" in #general: the reviewer answers in the thread under the message,
// at once with a message that says it is on it, and a read-only job writes the answer. The
// thread is an ask, and asking again changes nothing.
func TestAMentionStartsATurnInTheThread(t *testing.T) {
	w := newWorld(t)
	reviewer := w.agent("reviewer")
	msg := w.post(w.admin, w.general.ID, "@reviewer look at PR #12", reviewer)

	turn := w.turnOf(reviewer, msg)
	if turn.Status != TurnQueued || turn.Hop != 0 || turn.RequestedByKind != RequestedByUser || turn.RequestedBy != w.admin.UserID ||
		turn.OnBehalfOf != w.admin.UserID || !turn.JobID.Valid || !turn.ReplyMessageID.Valid || turn.Tier != TierChat {
		t.Fatalf("turn: %+v", turn)
	}
	job, err := w.q.GetJob(w.ctx, turn.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != "reply" || job.Tier != TierChat || job.AgentID != reviewer.ID || job.TurnID != turn.ID || job.Role != "reviewer" ||
		job.Instruction != "@reviewer look at PR #12" || job.CreatedByKind != "user" {
		t.Fatalf("job: %+v", job)
	}
	task, _ := w.q.GetTask(w.ctx, job.TaskID)
	if task.Kind != process.AskKind || task.AgentsAllowedAt.Valid {
		t.Fatalf("a mention outside a task asks in a hidden ask, with no consent given: %+v", task)
	}
	thread, err := w.q.GetConversation(w.ctx, turn.ConversationID)
	if err != nil || thread.ParentMessageID != msg.ID || thread.TaskID != task.ID {
		t.Fatalf("the answer goes in the thread under the message, the ask's: %+v %v", thread, err)
	}
	reply, _ := w.q.GetMessage(w.ctx, turn.ReplyMessageID)
	if reply.AuthorKind != conversations.AuthorAgent || reply.AuthorID != reviewer.ID || reply.Status != conversations.StatusStreaming ||
		reply.ConversationID != thread.ID || conversations.OriginOf(reply).Turn != uuidText(turn.ID) {
		t.Fatalf("reply: %+v", reply)
	}
	if w.count("SELECT count(*) FROM conversation_members WHERE conversation_id = $1 AND member_kind = 'agent' AND member_id = $2", thread.ID, reviewer.ID) != 1 {
		t.Fatal("the agent is in the thread it answers in")
	}

	// The same event again (the outbox delivers at least once) starts nothing new.
	w.dispatch(msg)
	if n := w.count("SELECT count(*) FROM agent_turns WHERE trigger_message_id = $1", msg.ID); n != 1 {
		t.Fatalf("%d turns", n)
	}
	if n := w.count("SELECT count(*) FROM jobs WHERE task_id = $1", task.ID); n != 1 {
		t.Fatalf("%d jobs", n)
	}
	if n := len(w.messagesIn(thread.ID)); n != 1 {
		t.Fatalf("%d messages in the thread", n)
	}
}

// What a person asks in a DM with an agent asks the agent, without a mention; the answer is in
// the DM.
func TestADMWithAnAgentAsksIt(t *testing.T) {
	w := newWorld(t)
	planner := w.agent("planner")
	dm, _, err := w.chat.Directory.OpenDM(w.ctx, w.admin, nil, w.project, planner.ID)
	if err != nil {
		t.Fatal(err)
	}
	msg := w.post(w.admin, dm.ID, "How would you split the billing work?")
	turn := w.turnOf(planner, msg)
	if turn.ConversationID != dm.ID || !turn.JobID.Valid {
		t.Fatalf("turn: %+v", turn)
	}
}

// A refused turn says why where the answer would have been, once, and nothing runs.
func TestRefusalsAreSaid(t *testing.T) {
	cases := []struct {
		name   string
		set    func(w *world)
		code   string
		saying string
	}{
		{"a paused agent", func(w *world) {
			if _, err := w.pool.Exec(w.ctx, "UPDATE agents SET status = 'paused' WHERE project_id = $1 AND handle = 'reviewer'", w.project); err != nil {
				w.t.Fatal(err)
			}
		}, RefusedPaused, "is paused"},
		{"a spent day", func(w *world) {
			w.policy(`{"policy":{"daily_budget_usd":1}}`)
			seed := w.post(w.admin, w.general.ID, "warm up", w.agent("planner"))
			w.spend(w.turnOf(w.agent("planner"), seed).JobID, 1.5)
		}, RefusedBudget, "daily budget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.set(w)
			reviewer := w.agent("reviewer")
			msg := w.post(w.admin, w.general.ID, "@reviewer what do you think?", reviewer)
			turn := w.turnOf(reviewer, msg)
			if turn.Status != TurnRefused || turn.Refusal == nil || *turn.Refusal != tc.code || turn.JobID.Valid {
				t.Fatalf("turn: %+v", turn)
			}
			said := w.settled(turn.ConversationID, func(m []db.Message) bool { return len(m) > 0 })
			if len(said) != 1 || said[0].AuthorKind != conversations.AuthorSystem || !strings.Contains(said[0].Body, tc.saying) {
				t.Fatalf("said: %+v", said)
			}
			w.dispatch(msg)
			if n := len(w.messagesIn(turn.ConversationID)); n != 1 {
				t.Fatal("a refusal is said once")
			}
		})
	}
}

// A viewer reads and reacts but writes nothing, so asks nobody: the message cannot even be
// posted. The guard stands for one who lost the right between posting and the turn.
func TestAViewerCannotAsk(t *testing.T) {
	w := newWorld(t)
	_, err := w.chat.Messages.Post(w.ctx, w.viewer, w.general.ID, conversations.Draft{Body: "@reviewer hi", Mentions: mentioning(w.agent("reviewer"))})
	if err != conversations.ErrForbidden {
		t.Fatalf("a viewer posted: %v", err)
	}
}

// @worker works only where a person allowed agents: outside that it answers in chat, read-only.
func TestAWorkAgentWorksOnlyWithConsent(t *testing.T) {
	w := newWorld(t)
	worker := w.agent("worker")
	msg := w.post(w.admin, w.general.ID, "@worker fix the flaky test", worker)
	turn := w.turnOf(worker, msg)
	job, _ := w.q.GetJob(w.ctx, turn.JobID)
	if turn.Tier != TierChat || job.Tier != TierChat {
		t.Fatalf("without consent @worker chats: turn %s job %s", turn.Tier, job.Tier)
	}

	// In a task a person allowed agents on, it works.
	task, err := w.svc.Create(w.ctx, process.CreateParams{ProjectID: w.project, Kind: "bug", Title: "Flaky"}, process.Actor{Kind: process.ActorUser, ID: w.admin.UserID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.AllowAgents(w.ctx, task.ID, process.Actor{Kind: process.ActorUser, ID: w.admin.UserID}); err != nil {
		t.Fatal(err)
	}
	thread, err := w.chat.Threads.ForTask(w.ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	in := w.post(w.admin, thread.ID, "@worker fix it", worker)
	turn = w.turnOf(worker, in)
	job, _ = w.q.GetJob(w.ctx, turn.JobID)
	if turn.Tier != TierWork || job.Tier != TierWork || job.TaskID != task.ID || job.Kind != "reply" {
		t.Fatalf("with consent @worker works on the task: turn %s job %+v", turn.Tier, job)
	}
	// The answer in the thread is not the phase's job: the autopilot still queues that one, the
	// inbox does not take the reply for it, and the task's kind may still change.
	phase, created, err := w.mgr.EnsureJob(w.ctx, task.ID)
	if err != nil || !created || phase.Kind != "phase" || phase.Role != "planner" {
		t.Fatalf("the autopilot: %+v %v %v", phase, created, err)
	}
	if worked, _ := w.q.TaskWorkedOn(w.ctx, task.ID); !worked {
		t.Fatal("a phase job is work on the task")
	}
	other, _ := w.svc.Create(w.ctx, process.CreateParams{ProjectID: w.project, Kind: "bug", Title: "Other"}, process.Actor{Kind: process.ActorUser, ID: w.admin.UserID})
	otherThread, _ := w.chat.Threads.ForTask(w.ctx, other)
	w.post(w.admin, otherThread.ID, "@reviewer is this a bug?", w.agent("reviewer"))
	if worked, _ := w.q.TaskWorkedOn(w.ctx, other.ID); worked {
		t.Fatal("an agent answering in a task's thread is no work on it")
	}
}

// A chain of agents stops: at its fourth turn, at its budget, and when an agent asks itself,
// each time with a line that says why.
func TestAChainOfAgentsStops(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		run    func(w *world, first db.AgentTurn) (db.Agent, db.Message)
		code   string
		saying string
	}{
		{"hop 4", `{}`, func(w *world, first db.AgentTurn) (db.Agent, db.Message) {
			planner, worker, reviewer := w.agent("planner"), w.agent("worker"), w.agent("reviewer")
			m1 := w.delegate(first, "@worker build it", worker)
			hop1 := w.turnOf(worker, m1)
			m2 := w.delegate(hop1, "@reviewer check it", reviewer)
			hop2 := w.turnOf(reviewer, m2)
			m3 := w.delegate(hop2, "@planner replan", planner)
			hop3 := w.turnOf(planner, m3)
			if hop1.Hop != 1 || hop2.Hop != 2 || hop3.Hop != 3 || hop3.ChainID != first.ChainID || hop3.Status != TurnQueued {
				w.t.Fatalf("hops: %d %d %d, chain kept %v, %s", hop1.Hop, hop2.Hop, hop3.Hop, hop3.ChainID == first.ChainID, hop3.Status)
			}
			if hop3.RequestedByKind != RequestedByAgent || hop3.RequestedBy != reviewer.ID || hop3.OnBehalfOf != w.admin.UserID {
				w.t.Fatalf("hop 3 was asked by @reviewer for the person who started it: %+v", hop3)
			}
			return worker, w.delegate(hop3, "@worker again", worker)
		}, RefusedHopLimit, "stopped at @worker"},
		{"the chain's budget", `{"policy":{"default":{"max_cost_usd":1}}}`, func(w *world, first db.AgentTurn) (db.Agent, db.Message) {
			w.spend(first.JobID, 2)
			worker := w.agent("worker")
			return worker, w.delegate(first, "@worker build it", worker)
		}, RefusedChainBudget, "spent $2.00 of its $2.00"},
		{"an agent asking itself", `{}`, func(w *world, first db.AgentTurn) (db.Agent, db.Message) {
			planner := w.agent("planner")
			return planner, w.delegate(first, "@planner think harder", planner)
		}, RefusedRepeat, "cannot ask itself"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.policy(tc.policy)
			planner := w.agent("planner")
			start := w.post(w.admin, w.general.ID, "@planner plan the billing work", planner)
			first := w.turnOf(planner, start)
			asked, msg := tc.run(w, first)
			turn := w.turnOf(asked, msg)
			if turn.Status != TurnRefused || turn.Refusal == nil || *turn.Refusal != tc.code || turn.JobID.Valid {
				t.Fatalf("turn: %+v", turn)
			}
			said := w.settled(turn.ConversationID, func(m []db.Message) bool { return m[len(m)-1].AuthorKind == conversations.AuthorSystem })
			last := said[len(said)-1]
			if last.AuthorKind != conversations.AuthorSystem || !strings.Contains(last.Body, tc.saying) {
				t.Fatalf("the thread should say why, it says %q", last.Body)
			}
			// A person replying starts a new chain, whatever the last one did.
			again := w.post(w.admin, turn.ConversationID, "@curator go on", w.agent("curator"))
			fresh := w.turnOf(w.agent("curator"), again)
			if fresh.Status != TurnQueued || fresh.Hop != 0 || fresh.ChainID == first.ChainID {
				t.Fatalf("a person's reply starts afresh: %+v", fresh)
			}
		})
	}
}
