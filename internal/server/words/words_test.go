package words

import "testing"

func TestWords(t *testing.T) {
	if got := OneLine("  a\nb\t\x00c  d ", 5); got != "a b c" {
		t.Errorf("%q", got)
	}
	if got := Clip("żółw", 2); got != "żó…" {
		t.Errorf("%q", got)
	}
	if _, ok := UUID("nope"); ok {
		t.Error("not an id")
	}
	if _, ok := UUID("00000000-0000-0000-0000-000000000001"); !ok {
		t.Error("an id")
	}
}
