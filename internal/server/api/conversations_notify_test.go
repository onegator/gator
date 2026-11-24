package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/notify"
	"github.com/onegator/gator/internal/server/store/db"
)

// pushes records what would have gone to each device.
type pushes struct {
	mu   sync.Mutex
	sent map[string][]notify.Message // device token -> messages
}

func (p *pushes) Send(_ context.Context, d db.Device, m notify.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent[d.Token] = append(p.sent[d.Token], m)
	return nil
}

func (p *pushes) Name() string { return "test" }

func (p *pushes) to(token string) []notify.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]notify.Message(nil), p.sent[token]...)
}

// device registers a phone for a person and answers its token.
func (h *harness) device(userID string) string {
	h.t.Helper()
	token := fmt.Sprintf("dev-%d", time.Now().UnixNano())
	if _, err := h.pool.Exec(context.Background(), "INSERT INTO devices (user_id, token, platform) VALUES ($1, $2, 'ios')", userID, token); err != nil {
		h.t.Fatal(err)
	}
	return token
}

// A mention, a DM and a reply in one's thread reach one's phone; a message in a channel one only
// reads does not.
func TestMentionsDMsAndRepliesArePushed(t *testing.T) {
	h := newHarness(t)
	rec := &pushes{sent: map[string][]notify.Message{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go notify.New(h.pool, h.api.Process, rec, h.api.Hub, slog.Default(), 50*time.Millisecond).WithChat(h.api.Conversations.Access).Run(ctx)
	time.Sleep(100 * time.Millisecond) // the notifier's cursor starts before anything is said

	c := h.crew("push")
	memberPhone, viewerPhone := h.device(c.memberID), h.device(c.viewerID)
	g, _ := h.general(c.admin, c.project.Id.String())
	conv := g.Id.String()

	h.post(c.admin, conv, gen.NewMessage{Body: ptr("Morning all")})
	h.post(c.admin, conv, gen.NewMessage{Body: ptr("Can you look at the login?"),
		Mentions: &[]gen.MemberRef{{Kind: gen.MemberRefKindUser, Id: mustOpenUUID(t, c.memberID)}}})
	eventually(t, "the mention reaching the member", func() bool {
		got := rec.to(memberPhone)
		return len(got) == 1 && strings.Contains(got[0].Title, "mentioned you in #general") && got[0].Body == "Can you look at the login?"
	})

	var dm gen.ConversationDetail
	h.as(c.stranger, func() {
		h.do("POST", "/conversations/dm", gen.NewDirectMessage{Members: []openapi_types.UUID{mustOpenUUID(t, c.memberID)}}, &dm)
	})
	h.post(c.stranger, dm.Id.String(), gen.NewMessage{Body: ptr("Got a minute?")})
	eventually(t, "the DM reaching the member", func() bool { return len(rec.to(memberPhone)) == 2 })

	_, root := h.post(c.member, conv, gen.NewMessage{Body: ptr("Shipping on Friday?")})
	var thread gen.ConversationDetail
	h.as(c.admin, func() { h.do("GET", "/messages/"+root.Id.String()+"/thread", nil, &thread) })
	h.post(c.admin, thread.Id.String(), gen.NewMessage{Body: ptr("Yes")})
	eventually(t, "the reply reaching the thread's author", func() bool {
		got := rec.to(memberPhone)
		return len(got) == 3 && strings.Contains(got[2].Title, "replied in a thread in #general")
	})
	time.Sleep(200 * time.Millisecond)
	if got := rec.to(viewerPhone); len(got) != 0 {
		t.Fatalf("the viewer, who was not mentioned, got %+v", got)
	}
}
