package client

import "testing"

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
