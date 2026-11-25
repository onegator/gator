package curator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

// CardBrainProposal is the card a task's thread shows of what the Brain was proposed from it: one
// card a day, listing that day's proposals, so that five of them are one thing to read rather than
// five cards.
const CardBrainProposal = "brain_proposal"

// cardStore is what the cards read. *db.Queries is one.
type cardStore interface {
	GetBrainEntry(ctx context.Context, id pgtype.UUID) (db.BrainEntry, error)
	GetTask(ctx context.Context, id pgtype.UUID) (db.Task, error)
	TaskBrainOfDay(ctx context.Context, arg db.TaskBrainOfDayParams) ([]db.BrainEntry, error)
	ListBrainLinks(ctx context.Context, ids []pgtype.UUID) ([]db.BrainLink, error)
}

// Cards is the card source of Brain proposals (conversations.CardSource).
type Cards struct{ store cardStore }

// NewCards builds it.
func NewCards(store cardStore) Cards { return Cards{store: store} }

var _ conversations.CardSource = Cards{}

// Events implements conversations.CardSource.
func (Cards) Events() []string {
	return []string{"brain.proposed", "brain.approved", "brain.rejected", "brain.archived", "brain.superseded"}
}

// Cards implements conversations.CardSource: the day's card of the task the entry came from.
// Nothing for an entry that came from no task, or from a private conversation — what was said
// there is not put in front of the whole project.
func (c Cards) Cards(ctx context.Context, e db.EventsAfterRow) ([]conversations.PlacedCard, error) {
	var payload struct {
		EntryID string `json:"entry_id"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return nil, nil
	}
	id, ok := brain.ParseUUID(payload.EntryID)
	if !ok {
		return nil, nil
	}
	entry, err := c.store.GetBrainEntry(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil || !entry.SourceTaskID.Valid || entry.SourcePrivate {
		return nil, err
	}
	task, err := c.store.GetTask(ctx, entry.SourceTaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	day := entry.CreatedAt.Time.UTC().Truncate(24 * time.Hour)
	entries, err := c.store.TaskBrainOfDay(ctx, db.TaskBrainOfDayParams{SourceTaskID: task.ID, Day: pgtype.Timestamptz{Time: day, Valid: true}})
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	card, err := c.card(ctx, task, day, entries)
	if err != nil {
		return nil, err
	}
	return []conversations.PlacedCard{{Task: task, Card: card}}, nil
}

// card is the day's card: every entry proposed from the task that day, where it stands, and for a
// proposal that would replace a decision, which one ("does this replace X?").
func (c Cards) card(ctx context.Context, task db.Task, day time.Time, entries []db.BrainEntry) (conversations.Card, error) {
	ids := make([]pgtype.UUID, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	links, err := c.store.ListBrainLinks(ctx, ids)
	if err != nil {
		return conversations.Card{}, err
	}
	contradicts := map[[16]byte][]string{}
	for _, l := range links {
		if l.Relation == "contradicts" && l.TargetKind == "entry" {
			contradicts[l.EntryID.Bytes] = append(contradicts[l.EntryID.Bytes], l.TargetID)
		}
	}
	open := 0
	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		item := map[string]any{"id": brain.UUIDString(e.ID), "kind": e.Kind, "scope": e.Scope, "title": e.Title, "status": e.Status}
		if e.ComponentKey != nil {
			item["component"] = *e.ComponentKey
		}
		if replaces := contradicts[e.ID.Bytes]; len(replaces) > 0 {
			item["replaces"] = replaces
		}
		if e.Status == brain.StatusProposed {
			open++
			item["actions"] = []string{"approve", "reject"}
		}
		items = append(items, item)
	}
	date := day.Format("2006-01-02")
	key := fmt.Sprintf("%s:%s", CardBrainProposal, date)
	state := "open"
	if open == 0 {
		state = "resolved"
	}
	body := fmt.Sprintf("Brain: %d proposal(s) from %s", len(entries), date)
	if open > 0 && open < len(entries) {
		body = fmt.Sprintf("Brain: %d proposal(s) from %s, %d waiting", len(entries), date, open)
	}
	return conversations.Card{Key: key, Type: CardBrainProposal, Body: body, Data: map[string]any{
		"ref":       conversations.Ref(CardBrainProposal, key, map[string]any{"taskId": brain.UUIDString(task.ID)}),
		"title":     "What the Brain was proposed",
		"state":     state,
		"summary":   fmt.Sprintf("%d proposal(s), %d waiting", len(entries), open),
		"day":       date,
		"proposals": items,
	}}, nil
}
