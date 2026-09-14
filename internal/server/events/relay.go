// Package events publishes the domain outbox to in-process subscribers (WebSocket hub,
// plugins, jobs). Operations never talk to subscribers directly; they insert into
// `events` and the Relay does the rest.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store/db"
)

// Event is one published domain event.
type Event struct {
	ID          int64           `json:"id"`
	Type        string          `json:"type"`
	Aggregate   string          `json:"aggregate"`
	AggregateID string          `json:"aggregateId,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   time.Time       `json:"createdAt"`
}

// Hub fans events out to topic subscribers. Topics: "inbox", "task:<id>", "job:<id>",
// "runners", "runner:<id>", "*".
type Hub struct {
	mu   sync.RWMutex
	subs map[string]map[chan Event]struct{}
}

// NewHub creates an empty hub.
func NewHub() *Hub { return &Hub{subs: map[string]map[chan Event]struct{}{}} }

// Subscribe returns a channel receiving events for the given topics and a cancel func.
func (h *Hub) Subscribe(topics ...string) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	for _, t := range topics {
		if h.subs[t] == nil {
			h.subs[t] = map[chan Event]struct{}{}
		}
		h.subs[t][ch] = struct{}{}
	}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		for _, t := range topics {
			delete(h.subs[t], ch)
		}
		h.mu.Unlock()
	}
}

// Publish delivers e to every subscriber of its topics. Slow subscribers drop events
// rather than block the relay; clients resync from the API.
func (h *Hub) Publish(e Event) {
	topics := []string{"*"}
	if e.Aggregate == "task" {
		topics = append(topics, "inbox", "task:"+e.AggregateID)
	}
	if e.Aggregate == "job" {
		topics = append(topics, "job:"+e.AggregateID)
	}
	if e.Aggregate == "runner" {
		topics = append(topics, "runners", "runner:"+e.AggregateID)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	seen := map[chan Event]bool{}
	for _, t := range topics {
		for ch := range h.subs[t] {
			if seen[ch] {
				continue
			}
			seen[ch] = true
			select {
			case ch <- e:
			default:
			}
		}
	}
}

// Relay polls the outbox and publishes unpublished rows in order.
type Relay struct {
	Pool     *pgxpool.Pool
	Hub      *Hub
	Interval time.Duration
	Log      *slog.Logger
}

// Run blocks until ctx is done.
func (r *Relay) Run(ctx context.Context) {
	if r.Interval == 0 {
		r.Interval = 500 * time.Millisecond
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		if err := r.Drain(ctx); err != nil && ctx.Err() == nil {
			r.Log.Warn("outbox relay", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Drain publishes every unpublished event once.
func (r *Relay) Drain(ctx context.Context) error {
	q := db.New(r.Pool)
	for {
		rows, err := q.ListUnpublishedEvents(ctx, 100)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			e := Event{ID: row.ID, Type: row.Type, Aggregate: row.Aggregate, Payload: row.Payload, CreatedAt: row.CreatedAt.Time}
			if row.AggregateID.Valid {
				v, _ := row.AggregateID.Value()
				e.AggregateID, _ = v.(string)
			}
			r.Hub.Publish(e)
			if err := q.MarkEventPublished(ctx, row.ID); err != nil {
				return err
			}
		}
	}
}
