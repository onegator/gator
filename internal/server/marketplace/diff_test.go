package marketplace

import (
	"strings"
	"testing"
)

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

func TestALongDiffIsBounded(t *testing.T) {
	long := strings.Repeat("line\n", 1000)
	if d := Diff(long, long+"more\n", "a", "b"); !strings.Contains(d, "too long to compare") {
		t.Fatal(d)
	}
}
