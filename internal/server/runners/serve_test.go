package runners

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/auth"
)

// dialServe connects a runner authenticated as p to m.serve.
func (f *fixture) dialServe(t *testing.T, p auth.Principal) (*wire, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		f.m.serve(context.Background(), c, p)
	}))
	t.Cleanup(srv.Close)
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return &wire{t: t, c: c}, done
}

func (f *fixture) principal(t *testing.T) auth.Principal {
	t.Helper()
	name := "runner-" + suffix()
	_, rec, err := auth.Tokens{Pool: f.pool}.Issue(context.Background(), auth.IssueParams{Kind: auth.KindRunner, Scope: "runner:" + name, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return auth.Principal{Kind: auth.KindRunner, TokenID: rec.ID, Name: name}
}

func TestServeRefusesARunnerThatDoesNotRegisterProperly(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		name string
		send func(w *wire)
		code string
	}{
		{"heartbeat first", func(w *wire) { w.send(proto.TypeHeartbeat, 1, proto.Heartbeat{}) }, "register_first"},
		{"garbage first", func(w *wire) { w.raw([]byte("{")) }, "register_first"},
		{"too old", func(w *wire) {
			b, _ := json.Marshal(proto.Envelope{Type: proto.TypeRegister, Seq: 1, Version: proto.MinSupportedVersion - 1, Payload: proto.Register{}})
			w.raw(b)
		}, "unsupported_version"},
		{"unreadable register", func(w *wire) { w.send(proto.TypeRegister, 1, "a string") }, "bad_register"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, done := f.dialServe(t, f.principal(t))
			c.send(w)
			env, ok := w.next(5 * time.Second)
			var e proto.Error
			if !ok || env.Type != proto.TypeError || env.Into(&e) != nil || e.Code != c.code || !e.Fatal {
				t.Fatalf("got %s %+v, want fatal %s", env.Type, e, c.code)
			}
			if env, ok := w.next(5 * time.Second); ok { // reading answers the close handshake
				t.Fatalf("after a fatal error the server said %s", env.Type)
			}
			if status := websocket.CloseStatus(w.closeErr); status != websocket.StatusPolicyViolation {
				t.Fatalf("closed with %v, want policy violation", status)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the server kept a refused runner connected")
			}
		})
	}
}

func TestServeRegistersAndForgetsARunner(t *testing.T) {
	f := newFixture(t)
	p := f.principal(t)
	w, done := f.dialServe(t, p)
	w.send(proto.TypeRegister, 1, proto.Register{Location: "moon", Capabilities: proto.Capabilities{Backends: []string{f.backend}}})
	env, ok := w.next(5 * time.Second)
	var reg proto.Registered
	if !ok || env.Type != proto.TypeRegistered || env.Into(&reg) != nil || reg.Version != proto.Version {
		t.Fatalf("got %s %+v", env.Type, reg)
	}
	id, _ := parseUUID(reg.RunnerID)
	if !f.m.Connected(id) {
		t.Fatal("registered runner is not connected")
	}
	r, err := f.q.GetRunner(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var caps proto.Capabilities
	_ = json.Unmarshal(r.Capabilities, &caps)
	if r.Name != p.Name || r.Location != "other" || r.Status != "online" || caps.MaxParallel != 1 {
		t.Fatalf("runner %q at %q is %q with %d slots", r.Name, r.Location, r.Status, caps.MaxParallel)
	}

	w.c.CloseNow()
	<-done
	if f.m.Connected(id) {
		t.Fatal("a closed runner is still connected")
	}
	if r, _ := f.q.GetRunner(context.Background(), id); r.Status != "offline" || r.OfflineReason != "disconnected" {
		t.Fatalf("after disconnect: %q %q", r.Status, r.OfflineReason)
	}
	if ev := f.runnerEvents(t, id); strings.Join(ev, ",") != "runner.online,runner.offline" {
		t.Fatalf("events %v", ev)
	}
}
