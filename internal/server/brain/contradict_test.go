package brain

import (
	"testing"

	"github.com/onegator/gator/internal/server/store/db"
)

func TestWhatMakesTwoDecisionsAlike(t *testing.T) {
	d := func(title, content string) db.BrainEntry { return db.BrainEntry{Title: title, Content: content} }
	cases := []struct {
		a, b db.BrainEntry
		want bool
	}{
		{d("Session tokens are JWT", "signed"), d("Session tokens are PASETO", "local"), true},
		{d("We use Postgres", "the workload is relational"), d("Use MySQL", "cheaper"), false},
		{d("Postgres for jobs", "River keeps the queue in the same database as the data"), d("Postgres everywhere", "the queue lives in the same database as the data, with River"), true},
		{d("Billing runs nightly", "at two"), d("Session tokens are JWT", "signed"), false},
	}
	for _, c := range cases {
		if got := alike(c.a, c.b); got != c.want {
			t.Errorf("%q ~ %q: %v, want %v", c.a.Title, c.b.Title, got, c.want)
		}
	}
	if w := significant("We use the PASETO v4 tokens, not JWT!"); len(w) != 3 || w[0] != "paseto" || w[2] != "jwt" {
		t.Fatalf("significant: %v", w)
	}
}
