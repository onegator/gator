package names

import "testing"

func TestNames(t *testing.T) {
	for s, want := range map[string]bool{"go-testing": true, "a": true, "Go": false, "-x": false, "x_y": false, "": false} {
		if Valid(s) != want {
			t.Errorf("%q", s)
		}
	}
	for s, want := range map[string]bool{"sentry": true, "sentry/get_issue": true, "sentry/": false, "Sentry/x": false, "s/a b": false} {
		if ValidGrant(s) != want {
			t.Errorf("grant %q", s)
		}
	}
}
