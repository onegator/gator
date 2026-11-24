package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/events"
)

// conversationRights remembers, for one WebSocket connection, what its caller may do in each
// conversation, for visibilityTTL: somebody taken out of a private channel stops hearing it
// within that long. It asks conversations.Access, the same rules as the REST API.
type conversationRights struct {
	access *conversations.Access
	p      auth.Principal
	now    func() time.Time
	known  map[string]rightsAt
}

type rightsAt struct {
	rights conversations.Rights
	at     time.Time
}

func newConversationRights(s *Server, p auth.Principal) *conversationRights {
	c := &conversationRights{p: p, now: time.Now, known: map[string]rightsAt{}}
	if s.Conversations != nil {
		c.access = s.Conversations.Access
	}
	return c
}

// of answers the caller's rights in a conversation; none when it cannot be told.
func (c *conversationRights) of(ctx context.Context, id string) conversations.Rights {
	if c.access == nil {
		return conversations.Rights{}
	}
	if r, ok := c.known[id]; ok && c.now().Sub(r.at) < visibilityTTL {
		return r.rights
	}
	var conv pgtype.UUID
	if err := conv.Scan(id); err != nil {
		return conversations.Rights{}
	}
	_, rights, err := c.access.RightsByID(ctx, conv, c.p)
	if err != nil {
		return conversations.Rights{} // fail closed, and do not cache a database hiccup
	}
	c.known[id] = rightsAt{rights: rights, at: c.now()}
	return rights
}

// typingEvery is how often one connection says it is typing in one conversation.
const typingEvery = 2 * time.Second

// typingRelay passes a client's "I am typing" to the others in the conversation. It goes to the
// hub alone: typing is gone as soon as it is said, so it is never written to the outbox.
type typingRelay struct {
	hub    *events.Hub
	rights *conversationRights
	p      auth.Principal
	last   map[string]time.Time
}

func newTypingRelay(s *Server, p auth.Principal) *typingRelay {
	return &typingRelay{hub: s.Hub, rights: newConversationRights(s, p), p: p, last: map[string]time.Time{}}
}

// say announces that the caller is typing in a conversation they may write in; anything else
// is dropped without a word.
func (t *typingRelay) say(ctx context.Context, conversation string) {
	if t.p.Kind != auth.KindUser || !t.p.UserID.Valid {
		return
	}
	now := t.rights.now()
	if at, ok := t.last[conversation]; ok && now.Sub(at) < typingEvery {
		return
	}
	if !t.rights.of(ctx, conversation).Write {
		return
	}
	t.last[conversation] = now
	payload, _ := json.Marshal(map[string]any{
		"conversationId": conversation,
		"member":         map[string]string{"kind": conversations.MemberUser, "id": uuidString(t.p.UserID)},
	})
	t.hub.Publish(events.Event{Type: conversations.EventTyping, Aggregate: conversations.Aggregate, AggregateID: conversation,
		Payload: payload, CreatedAt: now})
}
