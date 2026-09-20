package client

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A server URL that answers 404 is a configuration mistake, not a flaky network: a port that
// serves only webhooks looks exactly like this, and a runner aimed at one waits forever while
// its jobs queue up. The dial must name that, and must not give up either — a server being
// redeployed answers 404 for a moment too.
func TestWrongAddressIsToldApartFromANetworkBlip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(Config{ServerURL: srv.URL, Token: "t", Name: "test", Backends: []string{"fake"}}, nil, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registered, err := c.session(ctx)
	if registered {
		t.Fatal("a 404 cannot register")
	}
	if !errors.Is(err, errWrongAddress) {
		t.Fatalf("404 should report the address, got %v", err)
	}
	if errors.Is(err, ErrFatal) {
		t.Fatal("a 404 must keep retrying: a server mid-deploy answers one too")
	}
}

func TestWebSocketURL(t *testing.T) {
	cases := map[string]string{
		"https://gator.tail0b562b.ts.net":  "wss://gator.tail0b562b.ts.net/api/v1/runner",
		"https://gator.tail0b562b.ts.net/": "wss://gator.tail0b562b.ts.net/api/v1/runner",
		"http://localhost:8080":            "ws://localhost:8080/api/v1/runner",
		"wss://example.com/custom/runner":  "wss://example.com/custom/runner",
	}
	for in, want := range cases {
		got, err := WebSocketURL(in)
		if err != nil || got != want {
			t.Errorf("%s → %s (%v), want %s", in, got, err, want)
		}
	}
	if _, err := WebSocketURL("ftp://x"); err == nil {
		t.Error("ftp accepted")
	}
}

func TestSplitList(t *testing.T) {
	if got := SplitList(" claude, ,codex "); len(got) != 2 || got[0] != "claude" || got[1] != "codex" {
		t.Fatalf("%v", got)
	}
}
