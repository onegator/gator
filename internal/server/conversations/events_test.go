package conversations

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/events"
)

type fakeMembers []pgtype.UUID

func (f fakeMembers) ListConversationUserIDs(context.Context, pgtype.UUID) ([]pgtype.UUID, error) {
	return f, nil
}

// The relay learns whom an event concerns from the conversation's members as it publishes, so
// no stored event carries the member list.
func TestRelayAudience(t *testing.T) {
	ann, bob, cid := newID(1), newID(2), newID(3)
	r := NewRelayAudience(fakeMembers{ann, bob})
	ev := func(payload map[string]any) events.Event {
		b, _ := json.Marshal(payload)
		return events.Event{Aggregate: Aggregate, AggregateID: uuidString(newID(9)), Payload: b}
	}
	got, personal, err := r.Audience(context.Background(), ev(map[string]any{"also": []string{uuidString(cid), uuidString(ann)}}))
	if err != nil || personal || len(got) != 3 || !slices.Contains(got, uuidString(cid)) {
		t.Fatalf("members and the mentioned: %v %v %v", got, personal, err)
	}
	got, personal, _ = r.Audience(context.Background(), ev(map[string]any{"also": []string{uuidString(bob)}, "personal": true}))
	if !personal || len(got) != 1 || got[0] != uuidString(bob) {
		t.Fatalf("a personal event: %v %v", got, personal)
	}
	if got, _, _ := r.Audience(context.Background(), events.Event{Aggregate: "task"}); got != nil {
		t.Fatalf("a task event has an audience: %v", got)
	}
}
