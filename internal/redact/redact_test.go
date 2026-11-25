package redact

import "testing"

func TestValuesMasksKnownSecretsOnly(t *testing.T) {
	got := Values(`{"text":"key sk-ant-api03-abcdefgh and ok"}`, []string{"sk-ant-api03-abcdefgh", "ok", ""})
	if got != `{"text":"key [redacted] and ok"}` {
		t.Fatal(got)
	}
	if u := Union([]string{"a", "b"}, []string{"b", "c"}); len(u) != 3 || u[2] != "c" {
		t.Fatal(u)
	}
}
