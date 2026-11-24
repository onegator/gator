package conversations

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Events written to the outbox. All of them have aggregate "conversation" and the conversation's
// id, so they reach the topic conversation:<id>; their payload's "audience" lists the people they
// concern, which is what the "me" topic follows. Message bodies are never in an event: a client
// reads the message by its seq, through the same access rules as any other read.
const (
	EventConversationCreated = "conversation.created"
	EventConversationUpdated = "conversation.updated"
	EventMemberChanged       = "conversation.member_changed"
	EventConversationRead    = "conversation.read"
	EventMessagePosted       = "message.posted"
	EventMessageEdited       = "message.edited"
	EventMessageDeleted      = "message.deleted"
	// EventMessageReplied is a reply under a message: its reply count changed. It is written on
	// the conversation the root message lives in, so those reading it see the count move.
	EventMessageReplied  = "message.replied"
	EventReactionChanged = "reaction.changed"
	// EventTyping is never written: it goes straight to the hub and is gone.
	EventTyping = "conversation.typing"
)

// Aggregate is what the outbox calls a conversation.
const Aggregate = "conversation"

// emit records an event about conv in the outbox, in the caller's transaction. Its audience is
// the people in the conversation and whoever else is named (a person just removed, or mentioned).
func emit(ctx context.Context, q *db.Queries, typ string, conv pgtype.UUID, payload map[string]any, also ...pgtype.UUID) error {
	members, err := q.ListConversationUserIDs(ctx, conv)
	if err != nil {
		return err
	}
	payload["conversationId"] = uuidString(conv)
	payload["audience"] = audience(members, also)
	b, _ := json.Marshal(payload)
	_, err = q.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: Aggregate, AggregateID: conv, Payload: b})
	return err
}

// audience merges people without repeating anyone.
func audience(lists ...[]pgtype.UUID) []string {
	seen := map[[16]byte]bool{}
	out := []string{}
	for _, l := range lists {
		for _, id := range l {
			if id.Valid && !seen[id.Bytes] {
				seen[id.Bytes] = true
				out = append(out, uuidString(id))
			}
		}
	}
	return out
}

func memberPayload(m Member) map[string]any {
	return map[string]any{"kind": m.Kind, "id": uuidString(m.ID)}
}
