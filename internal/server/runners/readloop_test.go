package runners

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/onegator/gator/internal/proto"
)

// wire is the runner's end of a session whose read loop runs on the server's end.
type wire struct {
	t        *testing.T
	c        *websocket.Conn
	closeErr error // why the last read failed
}

func (f *fixture) connect(t *testing.T) *wire {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		f.s.conn = c
		f.s.readLoop()
	}))
	t.Cleanup(srv.Close)
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return &wire{t: t, c: c}
}

func (w *wire) send(typ proto.MessageType, seq uint64, payload any) {
	w.t.Helper()
	b, err := proto.Encode(typ, seq, payload)
	if err != nil {
		w.t.Fatal(err)
	}
	w.raw(b)
}

func (w *wire) raw(b []byte) {
	w.t.Helper()
	if err := w.c.Write(context.Background(), websocket.MessageText, b); err != nil {
		w.t.Fatal(err)
	}
}

// next reads what the server says next; ok is false when it says nothing for a while.
func (w *wire) next(wait time.Duration) (proto.RawEnvelope, bool) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, b, err := w.c.Read(ctx)
	if err != nil {
		w.closeErr = err
		return proto.RawEnvelope{}, false
	}
	env, err := proto.Decode(b)
	if err != nil {
		w.t.Fatal(err)
	}
	return env, true
}

// expect reads the next messages and checks their types, and for errors and acks, their detail.
func (w *wire) expect(want ...string) {
	w.t.Helper()
	for _, x := range want {
		env, ok := w.next(5 * time.Second)
		if !ok {
			w.t.Fatalf("waited for %s, got nothing", x)
		}
		got := string(env.Type)
		switch env.Type {
		case proto.TypeError:
			var e proto.Error
			_ = env.Into(&e)
			got += ":" + e.Code
		case proto.TypeAck:
			var a proto.Ack
			_ = env.Into(&a)
			got += ":" + strings.TrimSpace(strings.Repeat("|", int(a.Seq)))
		case proto.TypeLease:
			var l proto.Lease
			_ = env.Into(&l)
			got += ":" + strings.Repeat("j", len(l.Jobs))
		}
		if got != x {
			w.t.Fatalf("got %s, want %s", got, x)
		}
	}
}

func TestReadLoopAnswersEveryKindOfMessage(t *testing.T) {
	f := newFixture(t)
	job := f.leasedJob(t, f.s)
	w := f.connect(t)

	w.raw([]byte("not json"))
	w.expect("error:bad_message")

	w.send("gossip", 1, nil)
	w.expect("error:unknown_type")

	// A lease request is always answered, even with nothing to hand out.
	w.send(proto.TypeLeaseRequest, 2, proto.LeaseRequest{Slots: 1})
	w.expect("lease:")

	// Unreadable payloads are acked: resending them would not make them readable.
	w.send(proto.TypeEvents, 3, "not an object")
	w.expect("error:bad_events", "ack:|||")
	w.send(proto.TypeFinish, 4, "not an object")
	w.expect("error:bad_finish", "ack:||||")
	w.send(proto.TypeTurnEnding, 5, "not an object")
	w.expect("error:bad_turn_ending", "ack:|||||")

	// A refusal is final, so it is acked too.
	w.send(proto.TypeEvents, 6, proto.Events{JobID: "00000000-0000-0000-0000-000000000001"})
	w.expect("error:events_rejected", "ack:||||||")

	w.send(proto.TypeEvents, 7, proto.Events{JobID: uuidString(job.ID), Events: []proto.JobEvent{{Seq: 1, Type: "assistant"}}})
	w.expect("ack:|||||||")

	// A finish frees a slot; with nothing queued the server has nothing to offer and says nothing.
	w.send(proto.TypeFinish, 8, proto.Finish{JobID: uuidString(job.ID), Status: proto.StatusDone, Usage: usage})
	w.expect("ack:||||||||")
	w.send(proto.TypeLeaseRequest, 9, proto.LeaseRequest{Slots: 1})
	w.expect("lease:") // the first thing said after the ack: nothing was offered in between
	if got := f.job(t, job.ID); got.Status != proto.StatusDone {
		t.Fatalf("job is %q", got.Status)
	}

	// A heartbeat is not acked; a queued job is leased on it.
	next := f.queuedJob(t)
	w.send(proto.TypeHeartbeat, 10, proto.Heartbeat{})
	w.expect("lease:j")
	if got := f.job(t, next.ID); got.Status != "leased" || got.RunnerID != f.s.runnerID {
		t.Fatalf("job is %q for %v", got.Status, got.RunnerID)
	}
}
