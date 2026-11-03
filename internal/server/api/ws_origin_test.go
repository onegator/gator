package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// A browser page on another site carries the caller's session cookie but its own Origin; it
// may not open the event stream. The apps send no Origin, and the server's own pages and its
// public address send theirs.
func TestOnlyTheServersOwnPagesOpenTheEventStream(t *testing.T) {
	h := newHarness(t)
	h.api.PublicURL = "https://gator.example.ts.net:8443"
	u := strings.Replace(h.srv.URL, "http", "ws", 1) + "/api/v1/ws"
	open := func(origin string) int {
		t.Helper()
		header := http.Header{"Authorization": {"Bearer " + h.token}}
		if origin != "" {
			header.Set("Origin", origin)
		}
		c, resp, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{HTTPHeader: header})
		if err == nil {
			c.CloseNow()
			return http.StatusSwitchingProtocols
		}
		if resp == nil {
			t.Fatalf("dial %q: %v", origin, err)
		}
		return resp.StatusCode
	}
	for origin, want := range map[string]int{
		"":                                  http.StatusSwitchingProtocols, // Gator.app, the CLI, a runner
		h.srv.URL:                           http.StatusSwitchingProtocols, // the server's own pages
		"https://gator.example.ts.net:8443": http.StatusSwitchingProtocols, // GATOR_PUBLIC_URL
		"https://evil.example":              http.StatusForbidden,
		"https://gator.example.ts.net.evil.example": http.StatusForbidden,
	} {
		if got := open(origin); got != want {
			t.Errorf("Origin %q: %d, want %d", origin, got, want)
		}
	}
}
