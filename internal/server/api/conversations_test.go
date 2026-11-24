package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
