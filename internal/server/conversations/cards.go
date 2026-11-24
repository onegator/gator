package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"

	"github.com/onegator/gator/internal/server/store/db"
)

// Card types.
const (
	CardTask     = "task"
	CardDecision = "decision"
	CardJob      = "job"
	CardArtifact = "artifact"
)

// Card is a structured message about one object. It points at that object (data["ref"]) and
// clients read the object's current state from its own endpoint; the rest of data is a summary
// for the list. A card is identified in its conversation by Key: writing it again updates the
// message that is there rather than posting another.
type Card struct {
	Key  string
	Type string
	// Body is what a client that knows nothing of cards shows: Markdown, one line.
	Body string
	Data map[string]any
}

// Ref is what a card points at.
func Ref(kind, id string, extra map[string]any) map[string]any {
	ref := map[string]any{"kind": kind, "id": id}
	for k, v := range extra {
		ref[k] = v
	}
	return ref
}

// cardChange says what writing a card did.
type cardChange int

const (
	cardUnchanged cardChange = iota
	cardPosted
	cardEdited
)

// writeCard posts c into conv, or brings the card already there up to date; a card that says
// nothing new is left alone, so a redelivered event changes nothing. It emits message.posted or
// message.edited (with the new card) for whatever it changed.
func writeCard(ctx context.Context, q *db.Queries, conv db.Conversation, c Card) (db.Message, cardChange, error) {
	data, err := json.Marshal(c.Data)
	if err != nil {
		return db.Message{}, cardUnchanged, err
	}
	existing, err := q.GetCardForUpdate(ctx, db.GetCardForUpdateParams{ConversationID: conv.ID, CardKey: &c.Key})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return postCard(ctx, q, conv, c, data)
	case err != nil:
		return db.Message{}, cardUnchanged, err
	}
	if existing.Body == c.Body && sameJSON(existing.Card, data) {
		return existing, cardUnchanged, nil
	}
	msg, err := q.UpdateCard(ctx, db.UpdateCardParams{ID: existing.ID, Body: c.Body, Card: data})
	if err != nil {
		return db.Message{}, cardUnchanged, err
	}
	return msg, cardEdited, emit(ctx, q, EventMessageEdited, conv.ID, messagePayload(msg, nil))
}

func postCard(ctx context.Context, q *db.Queries, conv db.Conversation, c Card, data []byte) (db.Message, cardChange, error) {
	seq, err := q.NextMessageSeq(ctx, conv.ID)
	if err != nil {
		return db.Message{}, cardUnchanged, err
	}
	msg, err := q.InsertMessage(ctx, db.InsertMessageParams{
		ConversationID: conv.ID, Seq: seq, AuthorKind: AuthorSystem, Kind: messageCard, Body: c.Body,
		CardType: &c.Type, Card: data, CardKey: &c.Key, Origin: []byte(emptyOrigin), Status: messageFinal,
	})
	if err != nil {
		return db.Message{}, cardUnchanged, err
	}
	return msg, cardPosted, afterPost(ctx, q, conv, msg, System, nil)
}

// sameJSON compares two JSON documents by what they say, not how they are spelled: the database
// gives jsonb back in its own key order and spacing.
func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
