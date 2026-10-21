package client

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/onegator/gator/internal/proto"
)

type doneExecutor struct{ runs atomic.Int32 }

func (e *doneExecutor) Run(context.Context, proto.Job, JobIO) proto.Finish {
	e.runs.Add(1)
	return proto.Finish{Status: proto.StatusDone, Usage: proto.Usage{DurationMS: 1}}
}

// A runner started for one job takes it, delivers the receipt and exits, so the machine ends
// with the job instead of idling on somebody's bill.
func TestOnceExitsAfterItsJobIsDelivered(t *testing.T) {
	var registered proto.Register
	leases := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		send := func(t proto.MessageType, seq uint64, p any) {
			b, _ := proto.Encode(t, seq, p)
			_ = c.Write(ctx, websocket.MessageText, b)
		}
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			env, _ := proto.Decode(b)
			switch env.Type {
			case proto.TypeRegister:
				_ = env.Into(&registered)
				send(proto.TypeRegistered, 1, proto.Registered{RunnerID: "r", Version: proto.Version})
			case proto.TypeLeaseRequest:
				// Offer a job every time asked; a one-job runner must take only the first.
				n := leases.Add(1)
				send(proto.TypeLease, 2, proto.Lease{Jobs: []proto.Job{{JobID: "job-" + string(rune('0'+n)), Backend: "fake"}}})
			case proto.TypeFinish:
				send(proto.TypeAck, 3, proto.Ack{Seq: env.Seq})
			}
		}
	}))
	defer srv.Close()

	ex := &doneExecutor{}
	c := New(Config{ServerURL: srv.URL, Token: "t", Name: "once", Placement: proto.PlacementCloud, Backends: []string{"fake"},
		Once: true, MaxParallel: 4, HeartbeatInterval: 20 * time.Millisecond}, ex, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("the runner did not exit after its job")
	}
	if n := ex.runs.Load(); n != 1 {
		t.Fatalf("ran %d jobs, want 1", n)
	}
	if registered.Placement != proto.PlacementCloud || registered.Capabilities.MaxParallel != 1 {
		t.Fatalf("registered %+v", registered)
	}
}

func TestOnceGivesUpWhenNoJobComes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if env, _ := proto.Decode(b); env.Type == proto.TypeRegister {
				out, _ := proto.Encode(proto.TypeRegistered, 1, proto.Registered{RunnerID: "r", Version: proto.Version})
				_ = c.Write(r.Context(), websocket.MessageText, out)
			}
		}
	}))
	defer srv.Close()
	c := New(Config{ServerURL: srv.URL, Token: "t", Backends: []string{"fake"}, Once: true, OnceIdle: 200 * time.Millisecond},
		&doneExecutor{}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Run(ctx); err != nil || ctx.Err() != nil {
		t.Fatalf("an idle one-job runner did not exit: %v %v", err, ctx.Err())
	}
}
