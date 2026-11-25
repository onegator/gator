package api

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

func pgUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatal(err)
	}
	return u
}

// askIn opens the ask an agent's answer under this message would belong to, the way the
// dispatcher does: the message's thread becomes the ask's thread.
func (h *harness) askIn(token string, root gen.Message, by string) (db.Task, gen.ConversationDetail) {
	h.t.Helper()
	var thread gen.ConversationDetail
	h.as(token, func() { h.do("GET", "/messages/"+root.Id.String()+"/thread", nil, &thread) })
	ctx := context.Background()
	q := db.New(h.pool)
	conv, err := q.GetConversation(ctx, pgUUID(h.t, thread.Id.String()))
	if err != nil {
		h.t.Fatal(err)
	}
	msg, err := q.GetMessage(ctx, pgUUID(h.t, root.Id.String()))
	if err != nil {
		h.t.Fatal(err)
	}
	task, err := h.api.Conversations.Asks.For(ctx, conv, msg, process.Actor{Kind: process.ActorUser, ID: pgUUID(h.t, by)})
	if err != nil {
		h.t.Fatal(err)
	}
	return task, thread
}

func (h *harness) boardHas(token, project, task string) bool {
	h.t.Helper()
	var board []gen.Task
	h.as(token, func() { h.do("GET", "/projects/"+project+"/tasks", nil, &board) })
	for _, t := range board {
		if t.Id.String() == task {
			return true
		}
	}
	return false
}

// A conversation with an agent about no task is a hidden ask: it has the thread it was asked in
// and is on no board, in no inbox, has no card, and nobody makes one by hand.
func TestAnAskIsHidden(t *testing.T) {
	h := newHarness(t)
	c := h.crew("ask")
	general, _ := h.general(c.admin, c.project.Id.String())
	_, root := h.post(c.member, general.Id.String(), gen.NewMessage{Body: ptr("Why is the login page slow?")})
	ask, thread := h.askIn(c.member, root, c.memberID)
	if ask.Kind != process.AskKind || ask.Phase != "answer" || ask.Title != "Why is the login page slow?" {
		t.Fatalf("ask: %+v", ask)
	}
	var detail gen.ConversationDetail
	h.as(c.member, func() { h.do("GET", "/conversations/"+thread.Id.String(), nil, &detail) })
	if detail.Kind != gen.ConversationDetailKindTask || detail.TaskId == nil || detail.TaskId.String() != uuidText(ask.ID) {
		t.Fatalf("the thread should be the ask's: %+v", detail)
	}
	// Asked again in the same thread, it is the same ask.
	again, _ := h.askIn(c.member, root, c.memberID)
	if again.ID != ask.ID {
		t.Fatal("a second answer in a thread opened a second ask")
	}

	if h.boardHas(c.member, c.project.Id.String(), uuidText(ask.ID)) {
		t.Fatal("an ask is on the board")
	}
	var inbox []gen.Decision
	h.as(c.admin, func() { h.do("GET", "/inbox?projectId="+c.project.Id.String()+"&all=true", nil, &inbox) })
	for _, d := range inbox {
		if d.Task.Id.String() == uuidText(ask.ID) {
			t.Fatal("an ask is in the inbox")
		}
	}
	var templates []gen.ProjectTemplate
	h.as(c.admin, func() { h.do("GET", "/projects/"+c.project.Id.String()+"/templates", nil, &templates) })
	for _, tpl := range templates {
		if tpl.Kind == process.AskKind {
			t.Fatal("an ask is not a process a person picks")
		}
	}
	var e gen.Error
	h.as(c.member, func() {
		if code := h.do("POST", "/projects/"+c.project.Id.String()+"/tasks", map[string]any{"kind": "ask", "title": "x"}, &e); code != 400 {
			t.Fatalf("a person made an ask: %d %+v", code, e)
		}
	})
	var bug gen.Task
	h.as(c.member, func() {
		h.do("POST", "/projects/"+c.project.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "B"}, &bug)
	})
	h.as(c.member, func() {
		if code := h.do("PATCH", "/tasks/"+bug.Id.String(), map[string]any{"kind": "ask"}, &e); code != 400 {
			t.Fatalf("a task became an ask: %d %+v", code, e)
		}
	})
	if cards := h.cardsIn(c.member, thread.Id.String(), "task"); len(cards) != 0 {
		t.Fatalf("an ask has no task card: %+v", cards)
	}
}

// "Make this a task" on an ask changes its kind: the thread, the agents' jobs and their cost stay
// with the task, which enters its process and shows up on the board.
func TestPromotingAnAskKeepsItsHistory(t *testing.T) {
	h := newHarness(t)
	c := h.crew("askpromote")
	general, _ := h.general(c.admin, c.project.Id.String())
	_, root := h.post(c.member, general.Id.String(), gen.NewMessage{Body: ptr("Checkout times out for big carts")})
	ask, thread := h.askIn(c.member, root, c.memberID)
	_, reply := h.post(c.member, thread.Id.String(), gen.NewMessage{Body: ptr("Since Tuesday's deploy")})
	if _, err := h.pool.Exec(context.Background(), `INSERT INTO jobs (task_id, project_id, phase, role, backend, kind, tier, status)
		VALUES ($1, $2, 'answer', 'reviewer', 'claude', 'reply', 'chat', 'done')`, ask.ID, ask.ProjectID); err != nil {
		t.Fatal(err)
	}

	var out gen.PromotedMessage
	h.as(c.member, func() {
		if code := h.do("POST", "/messages/"+reply.Id.String()+"/promote", gen.PromoteMessage{Kind: "bug"}, &out); code != 201 {
			t.Fatalf("promote: %d", code)
		}
	})
	if out.Task.Id.String() != uuidText(ask.ID) || out.Task.Kind != "bug" || out.Task.Phase != "planning" || out.ConversationId != thread.Id {
		t.Fatalf("the ask should have become this bug, in its thread: %+v", out)
	}
	if out.Task.Title != "Since Tuesday's deploy" {
		t.Fatalf("title: %q", out.Task.Title)
	}
	if d := deref(out.Task.Description); !contains(d, "Checkout times out") || !contains(d, "Since Tuesday") {
		t.Fatalf("description: %s", d)
	}
	var jobs []gen.Job
	h.as(c.member, func() { h.do("GET", "/tasks/"+out.Task.Id.String()+"/jobs", nil, &jobs) })
	if len(jobs) != 1 || jobs[0].Kind != "reply" {
		t.Fatalf("the agent's answer is still the task's: %+v", jobs)
	}
	if !h.boardHas(c.member, c.project.Id.String(), uuidText(ask.ID)) {
		t.Fatal("the bug is on the board")
	}
	h.as(c.member, func() {
		var again gen.PromotedMessage
		if code := h.do("POST", "/messages/"+root.Id.String()+"/promote", gen.PromoteMessage{Kind: "feature"}, &again); code != 409 {
			t.Fatalf("a task is made a task once: %d %+v", code, again)
		}
	})
}

// In a DM, an ask belongs to the DM: one per DM, found again by it, and its cards go there.
func TestADMKeepsOneAsk(t *testing.T) {
	h := newHarness(t)
	c := h.crew("askdm")
	var dm gen.ConversationDetail
	h.as(c.admin, func() {
		pid := c.project.Id
		h.do("POST", "/conversations/dm", gen.NewDirectMessage{Members: []openapi_types.UUID{mustOpenUUID(t, c.memberID)}, ProjectId: &pid}, &dm)
	})
	_, msg := h.post(c.admin, dm.Id.String(), gen.NewMessage{Body: ptr("Can you look at the flaky test?")})
	ctx := context.Background()
	q := db.New(h.pool)
	conv, _ := q.GetConversation(ctx, pgUUID(t, dm.Id.String()))
	m, _ := q.GetMessage(ctx, pgUUID(t, msg.Id.String()))
	by := process.Actor{Kind: process.ActorUser, ID: pgUUID(t, c.memberID)}
	first, err := h.api.Conversations.Asks.For(ctx, conv, m, by)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.api.Conversations.Asks.For(ctx, conv, m, by)
	if err != nil || second.ID != first.ID {
		t.Fatalf("a DM keeps one ask: %v %v", first.ID, second.ID)
	}
	home, err := h.api.Conversations.Threads.ForTask(ctx, first)
	if err != nil || home.ID != conv.ID {
		t.Fatalf("the ask's conversation is the DM: %v %v", home.ID, err)
	}
}

func uuidText(u pgtype.UUID) string {
	v, _ := u.Value()
	s, _ := v.(string)
	return s
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
