package client

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/onegator/gator/internal/proto"
)

// capsExecutor is an executor whose backends' capabilities a test changes while it runs.
type capsExecutor struct {
	doneExecutor
	mu   sync.Mutex
	caps map[string]proto.BackendCapabilities
}

func (e *capsExecutor) BackendCapabilities() map[string]proto.BackendCapabilities {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]proto.BackendCapabilities{}
	for k, v := range e.caps {
		out[k] = v
	}
	return out
}

func (e *capsExecutor) set(name string, c proto.BackendCapabilities) {
	e.mu.Lock()
	e.caps[name] = c
	e.mu.Unlock()
}

// The registration says what each backend can do, and a heartbeat says it again only when it
// changed: a backend that fell back to print mode can do less than it said, and the server must
// know before it shows a Steer that no longer works.
func TestBackendCapabilitiesAreReportedAndUpdated(t *testing.T) {
	var mu sync.Mutex
	var registered proto.Register
	var beats []proto.Heartbeat
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
			env, _ := proto.Decode(b)
			mu.Lock()
			switch env.Type {
			case proto.TypeRegister:
				_ = env.Into(&registered)
				out, _ := proto.Encode(proto.TypeRegistered, 1, proto.Registered{RunnerID: "r", Version: proto.Version})
				_ = c.Write(r.Context(), websocket.MessageText, out)
			case proto.TypeHeartbeat:
				var hb proto.Heartbeat
				_ = env.Into(&hb)
				beats = append(beats, hb)
			}
			mu.Unlock()
		}
	}))
	defer srv.Close()

	live := proto.BackendCapabilities{ActiveSteering: true, NativeInterrupt: true, ToolApprovals: true, Enforcement: proto.EnforcementNative}
	ex := &capsExecutor{caps: map[string]proto.BackendCapabilities{"claude": live}}
	c := New(Config{ServerURL: srv.URL, Token: "t", Name: "caps", Backends: []string{"claude"}, HeartbeatInterval: 20 * time.Millisecond},
		ex, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			done := ok()
			mu.Unlock()
			if done {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	waitFor("three heartbeats", func() bool { return len(beats) >= 3 })
	mu.Lock()
	if got := registered.Capabilities.PerBackend["claude"]; got != live {
		t.Errorf("registered %+v, want %+v", got, live)
	}
	for _, hb := range beats {
		if hb.Capabilities != nil {
			t.Errorf("a heartbeat repeated unchanged capabilities: %+v", hb.Capabilities)
		}
	}
	seen := len(beats)
	mu.Unlock()

	print := proto.BackendCapabilities{Enforcement: proto.EnforcementNative}
	ex.set("claude", print)
	waitFor("the change", func() bool {
		for _, hb := range beats[seen:] {
			if hb.Capabilities != nil {
				return true
			}
		}
		return false
	})
	waitFor("two more heartbeats", func() bool { return len(beats) >= seen+3 })
	mu.Lock()
	defer mu.Unlock()
	changes := 0
	for _, hb := range beats[seen:] {
		if hb.Capabilities != nil {
			changes++
			if hb.Capabilities["claude"] != print {
				t.Errorf("heartbeat says %+v, want %+v", hb.Capabilities["claude"], print)
			}
		}
	}
	if changes != 1 {
		t.Errorf("the change was sent %d times, want once", changes)
	}
}
