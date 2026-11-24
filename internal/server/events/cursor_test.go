package events

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

func TestDrainCursorDeliversEachEventOnceInOrder(t *testing.T) {
	url := os.Getenv("GATOR_DATABASE_URL")
	if url == "" {
		t.Skip("GATOR_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := db.New(pool)

	suffix := time.Now().UnixNano()
	cursor, typ := fmt.Sprintf("test-%d", suffix), fmt.Sprintf("test.drained.%d", suffix)
	if _, err := q.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: "test", Payload: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if err := q.InitEventCursor(ctx, cursor); err != nil {
		t.Fatal(err)
	}
	// More than one batch, so the cursor has to move between them.
	var want []int64
	for range 150 {
		id, err := q.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: "test", Payload: []byte("{}")})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}

	var got []int64
	DrainCursor(ctx, pool, cursor, []string{typ}, func(r db.EventsAfterRow) { got = append(got, r.ID) })
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("delivered %d events, want the %d after the cursor, in order", len(got), len(want))
	}
	DrainCursor(ctx, pool, cursor, []string{typ}, func(r db.EventsAfterRow) { t.Fatalf("event %d delivered twice", r.ID) })
}

// A consumer that fails on an event gets that event again on the next drain: the cursor stays
// just before it, past everything delivered before it.
func TestDrainCursorUntilStopsAtTheFirstFailure(t *testing.T) {
	url := os.Getenv("GATOR_DATABASE_URL")
	if url == "" {
		t.Skip("GATOR_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := db.New(pool)
	suffix := time.Now().UnixNano()
	cursor, typ := fmt.Sprintf("until-%d", suffix), fmt.Sprintf("test.until.%d", suffix)
	if err := q.InitEventCursor(ctx, cursor); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for range 3 {
		id, err := q.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: "test", Payload: []byte("{}")})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	boom := fmt.Errorf("boom")
	var got []int64
	err = DrainCursorUntil(ctx, pool, cursor, []string{typ}, func(r db.EventsAfterRow) error {
		if r.ID == ids[1] {
			return boom
		}
		got = append(got, r.ID)
		return nil
	})
	if err != boom || fmt.Sprint(got) != fmt.Sprint(ids[:1]) {
		t.Fatalf("first drain: %v, delivered %v", err, got)
	}
	got = nil
	if err := DrainCursorUntil(ctx, pool, cursor, []string{typ}, func(r db.EventsAfterRow) error {
		got = append(got, r.ID)
		return nil
	}); err != nil || fmt.Sprint(got) != fmt.Sprint(ids[1:]) {
		t.Fatalf("second drain: %v, delivered %v, want the failed event and the rest", err, got)
	}
}
