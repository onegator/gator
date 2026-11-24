package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/events"
)

// as runs one call as the person holding token, and gives the harness its caller back.
func (h *harness) as(token string, call func()) {
	h.t.Helper()
	prev := h.token
	h.token = token
	defer func() { h.token = prev }()
	call()
}

// crew is a project with an admin, a member, a viewer and somebody outside it.
type crew struct {
	project                         gen.Project
	admin, member, viewer, stranger string // tokens
	memberID, viewerID, strangerID  string
}

func (h *harness) crew(name string) crew {
	h.t.Helper()
	_, admin, project := h.ownProject(name)
	c := crew{project: project, admin: admin}
	c.memberID, c.member = h.login("member")
	h.member(project.Id.String(), c.memberID, "member")
	c.viewerID, c.viewer = h.login("member")
	h.member(project.Id.String(), c.viewerID, "viewer")
	c.strangerID, c.stranger = h.login("member")
	return c
}

func (h *harness) channel(token, project, name, visibility string) gen.ConversationDetail {
	h.t.Helper()
	var out gen.ConversationDetail
	in := gen.NewChannel{Name: name}
	if visibility != "" {
		v := gen.NewChannelVisibility(visibility)
		in.Visibility = &v
	}
	h.as(token, func() {
		if code := h.do("POST", "/projects/"+project+"/conversations", in, &out); code != 201 {
			h.t.Fatalf("create #%s: %d", name, code)
		}
	})
	return out
}

func (h *harness) post(token, conversation string, in gen.NewMessage) (int, gen.Message) {
	h.t.Helper()
	var out gen.Message
	var code int
	h.as(token, func() { code = h.do("POST", "/conversations/"+conversation+"/messages", in, &out) })
	return code, out
}

// awaitWhere reads events until one of the given type satisfies ok.
func awaitWhere(t *testing.T, ws *websocket.Conn, want string, ok func(events.Event) bool) events.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, b, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", want, err)
		}
		var e events.Event
		_ = json.Unmarshal(b, &e)
		if e.Type == want && ok(e) {
			return e
		}
	}
}

// Two people talk in #general live: one posts, the other hears it on the conversation's topic
// and on "me", reads it by seq, and has it as unread until they mark it read.
func TestTwoPeopleTalkInGeneralLive(t *testing.T) {
	h := newHarness(t)
	c := h.crew("talk")
	ch, ok := h.general(c.admin, c.project.Id.String())
	if !ok {
		t.Fatal("the project has no #general")
	}
	conv := ch.Id.String()

	// The member joins, so unread is kept for them.
	h.as(c.member, func() {
		if code := h.do("PUT", "/conversations/"+conv+"/members/user/"+c.memberID, nil, nil); code != 200 {
			t.Fatalf("join: %d", code)
		}
	})
	var live, me *websocket.Conn
	h.as(c.member, func() {
		live = h.follow("conversation:" + conv)
		me = h.follow("me")
	})
	time.Sleep(100 * time.Millisecond)

	code, msg := h.post(c.admin, conv, gen.NewMessage{Body: ptr("Hello **team**"), ClientId: ptr("c-1"),
		Mentions: &[]gen.MemberRef{{Kind: gen.MemberRefKindUser, Id: mustOpenUUID(t, c.memberID)}}})
	if code != 201 || msg.Seq != 1 || msg.Body != "Hello **team**" || len(msg.Mentions) != 1 {
		t.Fatalf("post: %d %+v", code, msg)
	}
	e := await(t, live, "message.posted")
	if !strings.Contains(string(e.Payload), msg.Id.String()) || strings.Contains(string(e.Payload), "team") {
		t.Fatalf("the event names the message and keeps its words out: %s", e.Payload)
	}
	await(t, me, "message.posted")

	var page gen.MessagePage
	h.as(c.member, func() { h.do("GET", "/conversations/"+conv+"/messages?afterSeq=0", nil, &page) })
	if len(page.Messages) != 1 || page.Messages[0].Id != msg.Id {
		t.Fatalf("read after seq 0: %+v", page)
	}
	listed := func() gen.Conversation {
		t.Helper()
		var mine []gen.Conversation
		h.as(c.member, func() { h.do("GET", "/projects/"+c.project.Id.String()+"/conversations", nil, &mine) })
		for _, l := range mine {
			if l.Id == ch.Id {
				return l
			}
		}
		t.Fatalf("#general is not listed: %+v", mine)
		return gen.Conversation{}
	}
	if l := listed(); l.Unread != 1 || l.UnreadMentions != 1 {
		t.Fatalf("unread before reading: %+v", l)
	}
	h.as(c.member, func() {
		if code := h.do("POST", "/conversations/"+conv+"/read", gen.ReadMarker{Seq: 1}, nil); code != 200 {
			t.Fatalf("read: %d", code)
		}
	})
	if l := listed(); l.Unread != 0 || l.UnreadMentions != 0 {
		t.Fatalf("unread after reading: %+v", l)
	}
	await(t, me, "conversation.read")

	// Typing reaches the others and is never stored.
	h.as(c.admin, func() {
		ws := h.follow("conversation:" + conv)
		time.Sleep(50 * time.Millisecond)
		if err := ws.Write(context.Background(), websocket.MessageText, []byte(`{"typing":"`+conv+`"}`)); err != nil {
			t.Fatal(err)
		}
	})
	await(t, live, "conversation.typing")
	var stored int
	_ = h.pool.QueryRow(context.Background(), "SELECT count(*) FROM events WHERE type='conversation.typing'").Scan(&stored)
	if stored != 0 {
		t.Fatalf("typing was stored %d times", stored)
	}
}

// Posts that arrive at the same time are numbered one after another, without gaps.
func TestConcurrentPostsGetConsecutiveSeqs(t *testing.T) {
	h := newHarness(t)
	c := h.crew("seq")
	conv := h.channel(c.admin, c.project.Id.String(), "seq", "").Id.String()

	const n = 20
	seqs := make([]int64, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := c.admin
			if i%2 == 1 {
				token = c.member
			}
			seqs[i] = h.postRaw(token, conv, gen.NewMessage{Body: ptr(fmt.Sprintf("m%d", i))})
		}(i)
	}
	wg.Wait()
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, s := range seqs {
		if s != int64(i+1) {
			t.Fatalf("seqs %v: want 1..%d without gaps or repeats", seqs, n)
		}
	}
	var page gen.MessagePage
	h.as(c.member, func() { h.do("GET", "/conversations/"+conv+"/messages?limit=5", nil, &page) })
	if len(page.Messages) != 5 || page.Messages[0].Seq != n-4 || page.Messages[4].Seq != n || !page.HasMore {
		t.Fatalf("newest page: %+v", page)
	}
	h.as(c.member, func() {
		h.do("GET", fmt.Sprintf("/conversations/%s/messages?beforeSeq=%d&limit=50", conv, n-4), nil, &page)
	})
	if len(page.Messages) != n-5 || page.Messages[0].Seq != 1 || page.HasMore {
		t.Fatalf("older page: %d messages, first %d, more %v", len(page.Messages), page.Messages[0].Seq, page.HasMore)
	}
}

// postRaw posts from a goroutine without failing the test there, and answers the seq.
func (h *harness) postRaw(token, conversation string, in gen.NewMessage) int64 {
	var buf strings.Builder
	_ = json.NewEncoder(&buf).Encode(in)
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/conversations/"+conversation+"/messages", strings.NewReader(buf.String()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	defer res.Body.Close()
	var m gen.Message
	_ = json.NewDecoder(res.Body).Decode(&m)
	return m.Seq
}

// A client that sends the same message twice, even at the same moment, gets the one message
// back, and the conversation's numbering skips nothing.
func TestClientIDMakesPostingIdempotent(t *testing.T) {
	h := newHarness(t)
	c := h.crew("idem")
	conv := h.channel(c.admin, c.project.Id.String(), "idem", "").Id.String()

	code, first := h.post(c.member, conv, gen.NewMessage{Body: ptr("once"), ClientId: ptr("retry-me")})
	if code != 201 {
		t.Fatalf("first: %d", code)
	}
	code, again := h.post(c.member, conv, gen.NewMessage{Body: ptr("once"), ClientId: ptr("retry-me")})
	if code != 200 || again.Id != first.Id || again.Seq != first.Seq {
		t.Fatalf("again: %d %+v", code, again)
	}
	var wg sync.WaitGroup
	seqs := make([]int64, 8)
	for i := range seqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seqs[i] = h.postRaw(c.member, conv, gen.NewMessage{Body: ptr("racing"), ClientId: ptr("race")})
		}(i)
	}
	wg.Wait()
	for _, s := range seqs {
		if s != seqs[0] {
			t.Fatalf("one client id made several messages: %v", seqs)
		}
	}
	code, next := h.post(c.member, conv, gen.NewMessage{Body: ptr("next")})
	if code != 201 || next.Seq != 3 {
		t.Fatalf("the next message after two distinct ones: %d seq %d", code, next.Seq)
	}
}

// A viewer reads and reacts but does not write; a private channel and a DM outside the project
// are invisible to everyone outside them, the workspace admin included, on REST and WebSocket.
func TestConversationPermissionsOverHTTP(t *testing.T) {
	h := newHarness(t)
	workspaceAdmin := h.token
	c := h.crew("perm")
	open := h.channel(c.admin, c.project.Id.String(), "open", "").Id.String()
	secret := h.channel(c.member, c.project.Id.String(), "secret", "private").Id.String()

	if code, _ := h.post(c.viewer, open, gen.NewMessage{Body: ptr("hi")}); code != 403 {
		t.Fatalf("a viewer posted: %d", code)
	}
	h.as(c.viewer, func() {
		if code := h.do("GET", "/conversations/"+open+"/messages", nil, nil); code != 200 {
			t.Fatalf("a viewer reads: %d", code)
		}
		var d gen.ConversationDetail
		h.do("GET", "/conversations/"+open, nil, &d)
		if !d.Rights.Read || !d.Rights.React || d.Rights.Write {
			t.Fatalf("a viewer's rights: %+v", d.Rights)
		}
		if code := h.do("POST", "/projects/"+c.project.Id.String()+"/conversations", gen.NewChannel{Name: "mine"}, nil); code != 403 {
			t.Fatalf("a viewer opened a channel: %d", code)
		}
	})
	if code, _ := h.post(c.stranger, open, gen.NewMessage{Body: ptr("hi")}); code != 404 {
		t.Fatalf("a stranger posted: %d", code)
	}
	if code, _ := h.post(c.member, secret, gen.NewMessage{Body: ptr("psst")}); code != 201 {
		t.Fatalf("the private channel's owner posts: %d", code)
	}
	for name, token := range map[string]string{"project admin": c.admin, "workspace admin": workspaceAdmin, "viewer": c.viewer} {
		h.as(token, func() {
			if code := h.do("GET", "/conversations/"+secret+"/messages", nil, nil); code != 404 {
				t.Errorf("%s read the private channel: %d", name, code)
			}
		})
	}

	// A DM outside the project.
	var dm gen.ConversationDetail
	h.as(c.member, func() {
		if code := h.do("POST", "/conversations/dm", gen.NewDirectMessage{Members: []openapi_types.UUID{mustOpenUUID(t, c.strangerID)}}, &dm); code != 201 {
			t.Fatalf("open DM: %d", code)
		}
	})
	if dm.ProjectId != nil || len(dm.Members) != 2 {
		t.Fatalf("DM: %+v", dm)
	}
	if code, _ := h.post(c.stranger, dm.Id.String(), gen.NewMessage{Body: ptr("hello from outside")}); code != 201 {
		t.Fatalf("a person in the DM writes: %d", code)
	}
	for name, token := range map[string]string{"project admin": c.admin, "workspace admin": workspaceAdmin} {
		h.as(token, func() {
			if code := h.do("GET", "/conversations/"+dm.Id.String(), nil, nil); code != 404 {
				t.Errorf("%s read the DM: %d", name, code)
			}
		})
	}

	// The WebSocket keeps to the same rules: following an unreadable conversation closes the
	// socket, and the firehose does not carry it either.
	h.as(c.admin, func() {
		ws := h.follow("conversation:" + secret)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("following a private channel from outside: %v", err)
		}
	})
	var firehose *websocket.Conn
	h.as(workspaceAdmin, func() { firehose = h.follow("*") })
	time.Sleep(100 * time.Millisecond)
	h.post(c.member, secret, gen.NewMessage{Body: ptr("still secret")})
	_, visible := h.post(c.member, open, gen.NewMessage{Body: ptr("for everyone")})
	e := await(t, firehose, "message.posted")
	if !strings.Contains(string(e.Payload), visible.Id.String()) {
		t.Fatalf("the firehose carried the private channel's message first: %s", e.Payload)
	}
	h.as(c.member, func() {
		ws := h.follow("user:" + c.viewerID)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("following someone else's events: %v", err)
		}
	})
}

// A reply opens the thread under a message the first time somebody asks, and moves its count.
func TestThreadUnderAMessage(t *testing.T) {
	h := newHarness(t)
	c := h.crew("thread")
	conv := h.channel(c.admin, c.project.Id.String(), "thread", "").Id.String()
	_, root := h.post(c.member, conv, gen.NewMessage{Body: ptr("Should we ship on Friday?")})

	var thread, again gen.ConversationDetail
	h.as(c.admin, func() {
		if code := h.do("GET", "/messages/"+root.Id.String()+"/thread", nil, &thread); code != 200 {
			t.Fatalf("thread: %d", code)
		}
		h.do("GET", "/messages/"+root.Id.String()+"/thread", nil, &again)
	})
	if thread.Kind != gen.ConversationDetailKindThread || again.Id != thread.Id || len(thread.Members) != 1 {
		t.Fatalf("thread: %+v, again %v", thread, again.Id)
	}
	if code, _ := h.post(c.admin, thread.Id.String(), gen.NewMessage{Body: ptr("No.")}); code != 201 {
		t.Fatalf("reply: %d", code)
	}
	var m gen.Message
	h.as(c.member, func() { h.do("GET", "/messages/"+root.Id.String(), nil, &m) })
	if m.ReplyCount != 1 || m.ThreadId == nil || *m.ThreadId != thread.Id {
		t.Fatalf("root after a reply: %+v", m)
	}
	var replies gen.MessagePage
	h.as(c.admin, func() { h.do("GET", "/conversations/"+thread.Id.String()+"/messages", nil, &replies) })
	h.as(c.admin, func() {
		if code := h.do("GET", "/messages/"+replies.Messages[0].Id.String()+"/thread", nil, nil); code != 400 {
			t.Fatalf("a thread under a reply: %d", code)
		}
	})
	h.as(c.stranger, func() {
		if code := h.do("GET", "/messages/"+root.Id.String()+"/thread", nil, nil); code != 404 {
			t.Fatalf("a stranger opened the thread: %d", code)
		}
	})
}

func mustOpenUUID(t *testing.T, s string) openapi_types.UUID {
	t.Helper()
	u := mustUUID(t, s)
	return openapi_types.UUID(u.Bytes)
}

// general finds a project's #general in the caller's list.
func (h *harness) general(token, project string) (gen.Conversation, bool) {
	h.t.Helper()
	var list []gen.Conversation
	h.as(token, func() { h.do("GET", "/projects/"+project+"/conversations?kind=channel", nil, &list) })
	for _, c := range list {
		if c.Name != nil && *c.Name == "general" {
			return c, true
		}
	}
	return gen.Conversation{}, false
}

// eventually retries ok for a few seconds: the outbox consumer acts a moment after the change.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("%s did not happen", what)
}

// A project starts with #general and its creator in it; whoever joins the project later is put
// in it too.
func TestEveryProjectHasGeneral(t *testing.T) {
	h := newHarness(t)
	_, admin, project := h.ownProject("gen")
	g, ok := h.general(admin, project.Id.String())
	if !ok || !g.Joined || g.Visibility != gen.ConversationVisibilityProject {
		t.Fatalf("#general for the creator: %+v %v", g, ok)
	}
	newcomerID, newcomer := h.login("member")
	h.as(admin, func() {
		if code := h.do("PUT", "/projects/"+project.Id.String()+"/members", map[string]string{"userId": newcomerID, "role": "member"}, nil); code != 200 && code != 204 {
			t.Fatalf("add member: %d", code)
		}
	})
	eventually(t, "the newcomer joining #general", func() bool {
		g, ok := h.general(newcomer, project.Id.String())
		return ok && g.Joined
	})
	h.as(admin, func() {
		if code := h.do("PATCH", "/conversations/"+g.Id.String(), gen.ConversationPatch{Archived: ptr(true)}, nil); code != 409 {
			t.Fatalf("archiving #general: %d", code)
		}
	})
}

// Every new task has its thread: the replies under its card in #general, or in the channel the
// project names for tasks. Asking for it straight away, before the consumer gets to it, answers
// the same one thread.
func TestEveryNewTaskHasItsThread(t *testing.T) {
	h := newHarness(t)
	c := h.crew("taskthread")
	pid := c.project.Id.String()

	var task gen.Task
	h.as(c.member, func() {
		if code := h.do("POST", "/projects/"+pid+"/tasks", gen.NewTask{Kind: "bug", Title: "Login loops"}, &task); code != 201 {
			t.Fatalf("create task: %d", code)
		}
	})
	var thread, again gen.ConversationDetail
	h.as(c.viewer, func() {
		if code := h.do("GET", "/tasks/"+task.Id.String()+"/conversation", nil, &thread); code != 200 {
			t.Fatalf("task conversation: %d", code)
		}
	})
	if thread.Kind != gen.ConversationDetailKindTask || thread.TaskId == nil || *thread.TaskId != task.Id || thread.ParentMessageId == nil {
		t.Fatalf("task thread: %+v", thread)
	}
	time.Sleep(300 * time.Millisecond) // let the consumer see task.created too
	h.as(c.member, func() { h.do("GET", "/tasks/"+task.Id.String()+"/conversation", nil, &again) })
	if again.Id != thread.Id {
		t.Fatalf("a second thread for one task: %v and %v", thread.Id, again.Id)
	}

	g, _ := h.general(c.member, pid)
	var page gen.MessagePage
	h.as(c.member, func() { h.do("GET", "/conversations/"+g.Id.String()+"/messages", nil, &page) })
	cards := 0
	for _, m := range page.Messages {
		if m.CardType != nil && *m.CardType == "task" && m.Card != nil && m.Card.Ref.Id == task.Id.String() {
			cards++
			if m.Id != *thread.ParentMessageId {
				t.Fatalf("the thread hangs under %v, not the task's card %v", *thread.ParentMessageId, m.Id)
			}
		}
	}
	if cards != 1 {
		t.Fatalf("#general has %d cards for the task: %+v", cards, page.Messages)
	}
	var viaCard gen.ConversationDetail
	h.as(c.member, func() { h.do("GET", "/messages/"+thread.ParentMessageId.String()+"/thread", nil, &viaCard) })
	if viaCard.Id != thread.Id {
		t.Fatalf("the thread under the card is not the task's: %v", viaCard.Id)
	}

	// A project that names another channel for its tasks gets their cards there.
	work := h.channel(c.admin, pid, "work", "")
	if _, err := h.pool.Exec(context.Background(), `UPDATE projects SET process_config = '{"conversations":{"task_channel":"work"}}' WHERE id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	var other gen.Task
	h.as(c.member, func() { h.do("POST", "/projects/"+pid+"/tasks", gen.NewTask{Kind: "bug", Title: "Second"}, &other) })
	h.as(c.member, func() { h.do("GET", "/tasks/"+other.Id.String()+"/conversation", nil, &thread) })
	var root gen.Message
	h.as(c.member, func() { h.do("GET", "/messages/"+thread.ParentMessageId.String(), nil, &root) })
	if root.ConversationId != work.Id {
		t.Fatalf("the card went to %v, not #work %v", root.ConversationId, work.Id)
	}
	h.as(c.stranger, func() {
		if code := h.do("GET", "/tasks/"+other.Id.String()+"/conversation", nil, nil); code != 403 {
			t.Fatalf("a stranger read a task's thread: %d", code)
		}
	})
}

// cardsIn answers the cards of one type in a conversation.
func (h *harness) cardsIn(token, conversation, cardType string) []gen.Message {
	h.t.Helper()
	var page gen.MessagePage
	h.as(token, func() { h.do("GET", "/conversations/"+conversation+"/messages?limit=200", nil, &page) })
	var out []gen.Message
	for _, m := range page.Messages {
		if m.CardType != nil && *m.CardType == cardType {
			out = append(out, m)
		}
	}
	return out
}

func cardState(m gen.Message) string {
	if m.Card == nil || m.Card.State == nil {
		return ""
	}
	return *m.Card.State
}

// Work shows up in the task's thread as cards: the decision its gate waits for, approved from
// the card's own endpoint, its jobs and its documents. Each is one card brought up to date, never
// a new card per event, and an event delivered twice changes nothing.
func TestCardsAreUpdatedNotDuplicated(t *testing.T) {
	h := newHarness(t)
	c := h.crew("cards")
	ctx := context.Background()
	var task gen.Task
	h.as(c.member, func() {
		h.do("POST", "/projects/"+c.project.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "Cards"}, &task)
	})
	var thread gen.ConversationDetail
	h.as(c.member, func() { h.do("GET", "/tasks/"+task.Id.String()+"/conversation", nil, &thread) })
	conv := thread.Id.String()

	var pending gen.Message
	eventually(t, "a decision card for planning", func() bool {
		cards := h.cardsIn(c.member, conv, "decision")
		if len(cards) == 1 && cardState(cards[0]) == "pending" {
			pending = cards[0]
			return true
		}
		return false
	})
	if pending.Card.Actions == nil || (*pending.Card.Actions)[0] != "approve" || pending.Card.Ref.Id != task.Id.String() {
		t.Fatalf("decision card: %+v", pending.Card)
	}
	h.as(c.member, func() {
		if code := h.do("POST", "/tasks/"+task.Id.String()+"/approve", nil, nil); code != 200 {
			t.Fatalf("approve: %d", code)
		}
	})
	eventually(t, "the decision card saying it was decided", func() bool {
		cards := h.cardsIn(c.member, conv, "decision")
		return len(cards) == 1 && cards[0].Id == pending.Id && cardState(cards[0]) == "resolved" && cards[0].EditedAt != nil
	})

	// A job from queued to failed, its finish delivered twice.
	var job string
	if err := h.pool.QueryRow(ctx, `INSERT INTO jobs (task_id, project_id, phase, role, backend) VALUES ($1, $2, 'planning', 'planner', 'claude') RETURNING id`,
		task.Id.String(), c.project.Id.String()).Scan(&job); err != nil {
		t.Fatal(err)
	}
	event := func(typ, aggregate, id, payload string) {
		t.Helper()
		if _, err := h.pool.Exec(ctx, `INSERT INTO events (type, aggregate, aggregate_id, payload) VALUES ($1, $2, $3, $4)`, typ, aggregate, id, payload); err != nil {
			t.Fatal(err)
		}
	}
	event("job.queued", "job", job, `{}`)
	eventually(t, "a job card", func() bool {
		cards := h.cardsIn(c.member, conv, "job")
		return len(cards) == 1 && cardState(cards[0]) == "queued"
	})
	if _, err := h.pool.Exec(ctx, `UPDATE jobs SET status = 'failed' WHERE id = $1`, job); err != nil {
		t.Fatal(err)
	}
	event("job.finished", "job", job, `{}`)
	event("job.finished", "job", job, `{}`)
	eventually(t, "the job card failing", func() bool {
		cards := h.cardsIn(c.member, conv, "job")
		return len(cards) == 1 && cardState(cards[0]) == "failed"
	})

	// Two versions of a plan: one card at the newest; the runner's own working state: none.
	for v := 1; v <= 2; v++ {
		if _, err := h.pool.Exec(ctx, `INSERT INTO artifacts (task_id, phase, type, version, content) VALUES ($1, 'planning', 'plan', $2, 'x')`, task.Id.String(), v); err != nil {
			t.Fatal(err)
		}
		event("artifact.created", "task", task.Id.String(), fmt.Sprintf(`{"phase":"planning","type":"plan","version":%d}`, v))
	}
	event("artifact.created", "task", task.Id.String(), `{"phase":"task","type":"working_state","version":1}`)
	eventually(t, "the plan's card at v2", func() bool {
		cards := h.cardsIn(c.member, conv, "artifact")
		return len(cards) == 1 && cardState(cards[0]) == "v2"
	})
	time.Sleep(300 * time.Millisecond)
	if n := len(h.cardsIn(c.member, conv, "artifact")); n != 1 {
		t.Fatalf("%d artifact cards", n)
	}
	var page gen.MessagePage
	h.as(c.member, func() { h.do("GET", "/conversations/"+conv+"/messages?limit=200", nil, &page) })
	if len(page.Messages) != 3 {
		t.Fatalf("the thread holds %d messages, want a decision, a job and a plan", len(page.Messages))
	}
}

// "Make this a task": the thread under a message becomes the task's thread, the task quotes it
// and links back, and the same thread cannot become a second task.
func TestPromoteMessageToTask(t *testing.T) {
	h := newHarness(t)
	c := h.crew("promote")
	conv := h.channel(c.admin, c.project.Id.String(), "support", "").Id.String()
	_, root := h.post(c.member, conv, gen.NewMessage{Body: ptr("Login loops on Safari\nSeen it twice today.")})
	var thread gen.ConversationDetail
	h.as(c.admin, func() { h.do("GET", "/messages/"+root.Id.String()+"/thread", nil, &thread) })
	h.post(c.admin, thread.Id.String(), gen.NewMessage{Body: ptr("Same on 17.2")})

	h.as(c.viewer, func() {
		if code := h.do("POST", "/messages/"+root.Id.String()+"/promote", gen.PromoteMessage{Kind: "bug"}, nil); code != 403 {
			t.Fatalf("a viewer made a task: %d", code)
		}
	})
	var out gen.PromotedMessage
	h.as(c.member, func() {
		if code := h.do("POST", "/messages/"+root.Id.String()+"/promote", gen.PromoteMessage{Kind: "bug"}, &out); code != 201 {
			t.Fatalf("promote: %d", code)
		}
	})
	if out.Task.Title != "Login loops on Safari" || out.ConversationId != thread.Id || out.Task.Kind != "bug" {
		t.Fatalf("promoted: %+v", out)
	}
	if d := deref(out.Task.Description); !strings.Contains(d, "> Login loops on Safari") || !strings.Contains(d, "Same on 17.2") ||
		!strings.Contains(d, "gator://conversations/"+conv+"?seq=1") {
		t.Fatalf("description: %s", d)
	}
	var taskThread gen.ConversationDetail
	h.as(c.member, func() { h.do("GET", "/tasks/"+out.Task.Id.String()+"/conversation", nil, &taskThread) })
	if taskThread.Id != thread.Id || taskThread.Kind != gen.ConversationDetailKindTask {
		t.Fatalf("the task's thread is %v (%s), not the message's thread %v", taskThread.Id, taskThread.Kind, thread.Id)
	}
	eventually(t, "the task's card in the thread it came from", func() bool {
		return len(h.cardsIn(c.member, thread.Id.String(), "task")) == 1
	})
	h.as(c.member, func() {
		if code := h.do("POST", "/messages/"+root.Id.String()+"/promote", gen.PromoteMessage{Kind: "feature"}, nil); code != 409 {
			t.Fatalf("a second task from one thread: %d", code)
		}
	})
	// A message nobody replied to gets its thread with the task.
	_, lone := h.post(c.member, conv, gen.NewMessage{Body: ptr("Add dark mode")})
	h.as(c.member, func() {
		if code := h.do("POST", "/messages/"+lone.Id.String()+"/promote", gen.PromoteMessage{Kind: "feature", Title: ptr("Dark mode")}, &out); code != 201 {
			t.Fatalf("promote a lone message: %d", code)
		}
	})
	var under gen.ConversationDetail
	h.as(c.member, func() { h.do("GET", "/messages/"+lone.Id.String()+"/thread", nil, &under) })
	if under.Id != out.ConversationId || under.TaskId == nil || *under.TaskId != out.Task.Id || out.Task.Title != "Dark mode" {
		t.Fatalf("the lone message's thread: %+v, promoted %+v", under, out)
	}
}

// upload sends one file into a conversation as multipart/form-data.
func (h *harness) upload(token, conversation, name, content string) (int, gen.Attachment) {
	h.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("conversationId", conversation)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write([]byte(content))
	_ = mw.Close()
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	var out gen.Attachment
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (h *harness) download(token, id string) (int, string, http.Header) {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/attachments/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

// Reactions, edits, deletes and files keep to who may do what: a viewer reacts, only the author
// edits and only for a day, the author or a moderator deletes, and a file reaches only those who
// may read where it was sent.
func TestReactEditDeleteAndAttach(t *testing.T) {
	h := newHarness(t)
	c := h.crew("changes")
	conv := h.channel(c.admin, c.project.Id.String(), "changes", "").Id.String()
	_, msg := h.post(c.member, conv, gen.NewMessage{Body: ptr("first draft")})
	mid := msg.Id.String()

	h.as(c.viewer, func() {
		if code := h.do("PUT", "/messages/"+mid+"/reactions/"+url.PathEscape("👍"), nil, nil); code != 204 {
			t.Fatalf("a viewer reacts: %d", code)
		}
		h.do("PUT", "/messages/"+mid+"/reactions/"+url.PathEscape("👍"), nil, nil)
	})
	h.as(c.admin, func() { h.do("PUT", "/messages/"+mid+"/reactions/"+url.PathEscape("👍"), nil, nil) })
	var got gen.Message
	h.as(c.member, func() { h.do("GET", "/messages/"+mid, nil, &got) })
	if len(got.Reactions) != 1 || got.Reactions[0].Emoji != "👍" || got.Reactions[0].Count != 2 {
		t.Fatalf("reactions: %+v", got.Reactions)
	}
	h.as(c.viewer, func() { h.do("DELETE", "/messages/"+mid+"/reactions/"+url.PathEscape("👍"), nil, nil) })
	h.as(c.member, func() { h.do("GET", "/messages/"+mid, nil, &got) })
	if got.Reactions[0].Count != 1 {
		t.Fatalf("after taking one back: %+v", got.Reactions)
	}

	h.as(c.admin, func() {
		if code := h.do("PATCH", "/messages/"+mid, gen.MessagePatch{Body: "not mine"}, nil); code != 403 {
			t.Fatalf("someone else edited: %d", code)
		}
	})
	h.as(c.member, func() {
		if code := h.do("PATCH", "/messages/"+mid, gen.MessagePatch{Body: "second draft"}, &got); code != 200 || got.Body != "second draft" || got.EditedAt == nil {
			t.Fatalf("edit: %d %+v", code, got)
		}
	})
	if _, err := h.pool.Exec(context.Background(), "UPDATE messages SET created_at = now() - interval '25 hours' WHERE id = $1", mid); err != nil {
		t.Fatal(err)
	}
	h.as(c.member, func() {
		if code := h.do("PATCH", "/messages/"+mid, gen.MessagePatch{Body: "too late"}, nil); code != 409 {
			t.Fatalf("an edit after a day: %d", code)
		}
	})

	_, other := h.post(c.admin, conv, gen.NewMessage{Body: ptr("the admin's")})
	h.as(c.member, func() {
		if code := h.do("DELETE", "/messages/"+other.Id.String(), nil, nil); code != 403 {
			t.Fatalf("a member deleted someone else's message: %d", code)
		}
	})
	h.as(c.admin, func() {
		if code := h.do("DELETE", "/messages/"+mid, nil, nil); code != 204 {
			t.Fatalf("a moderator deletes: %d", code)
		}
		h.do("GET", "/messages/"+mid, nil, &got)
	})
	if got.DeletedAt == nil || got.Body != "" || got.Seq != msg.Seq {
		t.Fatalf("deleted: %+v", got)
	}

	// A file: uploaded, sent with a message, read by those who may read the channel.
	code, att := h.upload(c.member, conv, "notes.txt", "remember the milk")
	if code != 201 || att.MessageId != nil || att.SizeBytes != 17 {
		t.Fatalf("upload: %d %+v", code, att)
	}
	if code, _, _ := h.download(c.admin, att.Id.String()); code != 404 {
		t.Fatalf("someone read a file not sent yet: %d", code)
	}
	if code, _ := h.post(c.admin, conv, gen.NewMessage{Body: ptr("mine now"), Attachments: &[]openapi_types.UUID{att.Id}}); code != 400 {
		t.Fatalf("someone else sent the member's file: %d", code)
	}
	code, sent := h.post(c.member, conv, gen.NewMessage{Body: ptr("see attached"), Attachments: &[]openapi_types.UUID{att.Id}})
	if code != 201 || len(sent.Attachments) != 1 || sent.Attachments[0].Id != att.Id {
		t.Fatalf("post with a file: %d %+v", code, sent)
	}
	code, body, header := h.download(c.viewer, att.Id.String())
	if code != 200 || body != "remember the milk" || header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %d %q %v", code, body, header)
	}
	if code, _, _ := h.download(c.stranger, att.Id.String()); code != 404 {
		t.Fatalf("a stranger downloaded: %d", code)
	}
	if code, _ := h.upload(c.viewer, conv, "x.txt", "x"); code != 403 {
		t.Fatalf("a viewer uploaded: %d", code)
	}
}

// Search finds words only where the caller may read them: the private channel's members find
// what was said there, the project's admin does not, and a DM is its people's alone.
func TestSearchKeepsToReadableConversations(t *testing.T) {
	h := newHarness(t)
	workspaceAdmin := h.token
	c := h.crew("search")
	word := fmt.Sprintf("wdrożenie%d", time.Now().UnixNano())
	open := h.channel(c.admin, c.project.Id.String(), "open", "").Id.String()
	secret := h.channel(c.member, c.project.Id.String(), "secret", "private").Id.String()
	var dm gen.ConversationDetail
	h.as(c.member, func() {
		h.do("POST", "/conversations/dm", gen.NewDirectMessage{Members: []openapi_types.UUID{mustOpenUUID(t, c.strangerID)}}, &dm)
	})
	_, inOpen := h.post(c.member, open, gen.NewMessage{Body: ptr("Plan na " + word + " w piątek")})
	_, inSecret := h.post(c.member, secret, gen.NewMessage{Body: ptr(word + " przesuwamy")})
	_, inDM := h.post(c.stranger, dm.Id.String(), gen.NewMessage{Body: ptr("Co z " + word + "?")})
	_, gone := h.post(c.member, open, gen.NewMessage{Body: ptr(word + " skasowane")})
	h.as(c.member, func() { h.do("DELETE", "/messages/"+gone.Id.String(), nil, nil) })

	found := func(token string) map[string]bool {
		t.Helper()
		var hits []gen.MessageHit
		h.as(token, func() {
			if code := h.do("GET", "/search/messages?q="+url.QueryEscape(word), nil, &hits); code != 200 {
				t.Fatalf("search: %d", code)
			}
		})
		out := map[string]bool{}
		for _, hit := range hits {
			out[hit.Message.Id.String()] = true
			if hit.Conversation.Id != hit.Message.ConversationId {
				t.Fatalf("a hit's conversation: %+v", hit)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		token string
		want  []gen.Message
	}{
		{"the private channel's owner, in the DM", c.member, []gen.Message{inOpen, inSecret, inDM}},
		{"the project's admin", c.admin, []gen.Message{inOpen}},
		{"a viewer", c.viewer, []gen.Message{inOpen}},
		{"the workspace admin", workspaceAdmin, []gen.Message{inOpen}},
		{"somebody outside the project, in the DM", c.stranger, []gen.Message{inDM}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := found(tc.token)
			if len(got) != len(tc.want) {
				t.Fatalf("found %d messages, want %d", len(got), len(tc.want))
			}
			for _, m := range tc.want {
				if !got[m.Id.String()] {
					t.Fatalf("did not find %q", m.Body)
				}
			}
		})
	}
	var named []gen.Conversation
	h.as(c.member, func() { h.do("GET", "/projects/"+c.project.Id.String()+"/conversations?q=SEC", nil, &named) })
	if len(named) != 1 || *named[0].Name != "secret" {
		t.Fatalf("channels named like sec: %+v", named)
	}
}

// A message in a private channel or a DM becomes a task the whole project can follow: its
// thread is a new one under the task's card in the task channel, it quotes nothing said in
// private but the promoter's own words, and the private conversation gets a line pointing at it.
func TestPromoteFromPrivateConversationsKeepsThemPrivate(t *testing.T) {
	h := newHarness(t)
	c := h.crew("promotepriv")
	pid := c.project.Id.String()
	secret := h.channel(c.member, pid, "secret", "private").Id.String()
	h.as(c.member, func() {
		h.do("PUT", "/conversations/"+secret+"/members/user/"+c.memberID, nil, nil)
	})
	_, root := h.post(c.member, secret, gen.NewMessage{Body: ptr("Export leaks salary bands")})
	var thread gen.ConversationDetail
	h.as(c.member, func() { h.do("GET", "/messages/"+root.Id.String()+"/thread", nil, &thread) })
	h.post(c.member, thread.Id.String(), gen.NewMessage{Body: ptr("private reply with numbers 1234")})

	var out gen.PromotedMessage
	h.as(c.member, func() {
		if code := h.do("POST", "/messages/"+root.Id.String()+"/promote", gen.PromoteMessage{Kind: "bug"}, &out); code != 201 {
			t.Fatalf("promote: %d", code)
		}
	})
	if out.ConversationId == thread.Id {
		t.Fatal("the private thread became the task's thread")
	}
	if d := deref(out.Task.Description); strings.Contains(d, "1234") || !strings.Contains(d, "Export leaks") || strings.Contains(d, secret) {
		t.Fatalf("description: %s", d)
	}
	// The project's admin, who is not in the channel, follows the task.
	var taskThread gen.ConversationDetail
	h.as(c.admin, func() {
		if code := h.do("GET", "/tasks/"+out.Task.Id.String()+"/conversation", nil, &taskThread); code != 200 {
			t.Fatalf("the project reads the task's thread: %d", code)
		}
	})
	var root2 gen.Message
	h.as(c.admin, func() { h.do("GET", "/messages/"+taskThread.ParentMessageId.String(), nil, &root2) })
	if root2.CardType == nil || *root2.CardType != "task" {
		t.Fatalf("the task's thread hangs under %+v, not its card", root2)
	}
	var page gen.MessagePage
	h.as(c.member, func() { h.do("GET", "/conversations/"+secret+"/messages", nil, &page) })
	last := page.Messages[len(page.Messages)-1]
	if last.AuthorKind != gen.MessageAuthorKindSystem || !strings.Contains(last.Body, out.Task.Id.String()) {
		t.Fatalf("no line in the private channel points at the task: %+v", last)
	}

	// From a DM outside the project: the DM's other person, outside the project, gets nothing.
	var dm gen.ConversationDetail
	h.as(c.member, func() {
		h.do("POST", "/conversations/dm", gen.NewDirectMessage{Members: []openapi_types.UUID{mustOpenUUID(t, c.strangerID)}}, &dm)
	})
	_, said := h.post(c.stranger, dm.Id.String(), gen.NewMessage{Body: ptr("your login is broken")})
	h.as(c.member, func() {
		if code := h.do("POST", "/messages/"+said.Id.String()+"/promote", gen.PromoteMessage{Kind: "bug", Title: ptr("Login"), ProjectId: &c.project.Id}, &out); code != 201 {
			t.Fatalf("promote from a DM: %d", code)
		}
	})
	if strings.Contains(deref(out.Task.Description), "broken") {
		t.Fatalf("someone else's DM words went into the task: %s", deref(out.Task.Description))
	}
	h.as(c.stranger, func() {
		if code := h.do("GET", "/conversations/"+out.ConversationId.String(), nil, nil); code != 404 {
			t.Fatalf("somebody outside the project reads the task's thread: %d", code)
		}
	})
	for _, cards := range [][]gen.Message{h.cardsIn(c.member, dm.Id.String(), "task"), h.cardsIn(c.member, secret, "task")} {
		if len(cards) != 0 {
			t.Fatalf("a task card went into a private conversation: %+v", cards)
		}
	}
}

// A private channel named as the task channel is not used: a task belongs to the whole project,
// and its card goes to #general instead.
func TestAPrivateTaskChannelFallsBackToGeneral(t *testing.T) {
	h := newHarness(t)
	c := h.crew("privtasks")
	pid := c.project.Id.String()
	h.channel(c.member, pid, "hidden", "private")
	if _, err := h.pool.Exec(context.Background(), `UPDATE projects SET process_config = '{"conversations":{"task_channel":"hidden"}}' WHERE id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	var task gen.Task
	h.as(c.member, func() { h.do("POST", "/projects/"+pid+"/tasks", gen.NewTask{Kind: "bug", Title: "Where"}, &task) })
	var thread gen.ConversationDetail
	h.as(c.admin, func() {
		if code := h.do("GET", "/tasks/"+task.Id.String()+"/conversation", nil, &thread); code != 200 {
			t.Fatalf("the project's admin reads the task's thread: %d", code)
		}
	})
	var root gen.Message
	h.as(c.admin, func() { h.do("GET", "/messages/"+thread.ParentMessageId.String(), nil, &root) })
	g, _ := h.general(c.admin, pid)
	if root.ConversationId != g.Id {
		t.Fatalf("the card went to %v, not #general", root.ConversationId)
	}
}
