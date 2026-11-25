package marketplace

import "testing"

func TestDiffIsLineByLine(t *testing.T) {
	got := Diff("a\nb\nc\n", "a\nB\nc\nd\n", "old", "new")
	want := "--- old\n+++ new\n a\n-b\n+B\n c\n+d\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if Diff("same", "same", "a", "b") != "" {
		t.Fatal("the same text has no diff")
	}
	if !same(`{"a":1,"b":[2]}`, `{"b": [2], "a": 1}`) || same("x", "y") || !same("x\n", "x") {
		t.Fatal("same")
	}
}
