package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func pair(a, b Handler) (*Conn, *Conn, func()) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	ca := NewConn(ar, aw, a)
	cb := NewConn(br, bw, b)
	return ca, cb, func() { _ = aw.Close(); _ = bw.Close() }
}

func TestCallsGoBothWays(t *testing.T) {
	var peer *Conn
	a, b, stop := pair(
		func(ctx context.Context, m string, p json.RawMessage) (any, error) {
			if m == "core.echo" {
				var s string
				_ = json.Unmarshal(p, &s)
				return "core:" + s, nil
			}
			return nil, Errorf(CodeMethodNotFound, "no %s", m)
		},
		func(ctx context.Context, m string, p json.RawMessage) (any, error) {
			// The plugin calls back into the core while serving the core's request.
			var s string
			if err := peer.Call(ctx, "core.echo", "inner", &s); err != nil {
				return nil, err
			}
			return s + "+plugin", nil
		})
	defer stop()
	peer = b
	var out string
	if err := a.Call(context.Background(), "hook", nil, &out); err != nil || out != "core:inner+plugin" {
		t.Fatalf("%q %v", out, err)
	}
	var re *Error
	if err := b.Call(context.Background(), "core.missing", nil, nil); !errors.As(err, &re) || re.Code != CodeMethodNotFound {
		t.Fatalf("want method not found, got %v", err)
	}
}

func TestCallTimeoutAndPeerClose(t *testing.T) {
	block := make(chan struct{})
	a, _, stop := pair(nil, func(ctx context.Context, m string, p json.RawMessage) (any, error) {
		<-block
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.Call(ctx, "slow", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Call(context.Background(), "slow", nil, nil) }()
	time.Sleep(20 * time.Millisecond)
	stop() // the peer's process died
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending call on a closed connection: %v", err)
	}
	close(block)
}

func TestHandlerPanicBecomesError(t *testing.T) {
	a, _, stop := pair(nil, func(context.Context, string, json.RawMessage) (any, error) { panic("boom") })
	defer stop()
	var re *Error
	if err := a.Call(context.Background(), "x", nil, nil); !errors.As(err, &re) || re.Code != CodeInternal {
		t.Fatalf("%v", err)
	}
}

func TestConfigSchema(t *testing.T) {
	s, err := ParseConfigSchema(json.RawMessage(`{"type":"object","properties":{
		"repo":{"type":"string"},"token":{"type":"string","x-secret":true},"retries":{"type":"integer"},
		"mode":{"type":"string","enum":["fast","safe"]}},"required":["repo","token"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(map[string]any{"repo": "a/b", "token": "t", "retries": 2.0, "mode": "fast"}); err != nil {
		t.Fatal(err)
	}
	err = s.Validate(map[string]any{"repo": 1.0, "retries": 1.5, "mode": "x", "extra": true})
	for _, want := range []string{`"repo" must be string`, `"retries" must be integer`, `"mode" must be one of`, `unknown setting "extra"`, `"token" is required`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
	pub, sec := s.Split(map[string]any{"repo": "a/b", "token": "t"})
	if pub["token"] != nil || sec["token"] != "t" || pub["repo"] != "a/b" {
		t.Fatalf("%v %v", pub, sec)
	}
	if SecretEnv("api-token") != "GATOR_SECRET_API_TOKEN" {
		t.Fatal(SecretEnv("api-token"))
	}
	if _, err := ParseConfigSchema(json.RawMessage(`{"properties":{"n":{"type":"integer","x-secret":true}}}`)); err == nil {
		t.Fatal("a secret must be a string")
	}
}

func TestCoreSatisfies(t *testing.T) {
	for _, c := range []struct {
		core, min string
		ok        bool
	}{{"1.2.3", "1.2.0", true}, {"1.2.3", "1.3.0", false}, {"v2.0.0", "1.9.9", true}, {"0.0.0-SNAPSHOT-abc", "9.0.0", true}, {"dev", "1.0.0", true}, {"1.0.0", "", true}} {
		if CoreSatisfies(c.core, c.min) != c.ok {
			t.Errorf("%s >= %s: want %v", c.core, c.min, c.ok)
		}
	}
}
