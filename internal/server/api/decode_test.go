package api

import (
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// A body that leaves out a field the spec requires is refused, whatever its type's zero value.
func TestMissingRequired(t *testing.T) {
	for _, c := range []struct {
		body string
		v    any
		want string
	}{
		{`{}`, &gen.ProjectConfig{}, "config"},
		{`{"config": null}`, &gen.ProjectConfig{}, "config"},
		{`{"config": {}}`, &gen.ProjectConfig{}, ""},
		{`{}`, &gen.FocusSetting{}, "enabled"},
		{`{"enabled": false}`, &gen.FocusSetting{}, ""},
		{`{"Enabled": true}`, &gen.FocusSetting{}, ""}, // the decoder takes it, so it was sent
		{`{"ENABLED": null}`, &gen.FocusSetting{}, "enabled"},
		{`{}`, &gen.AgentA2A{}, "exposed"},
		{`{"name": "x"}`, &gen.NewA2AClient{}, ""}, // the rest is optional
	} {
		if got := missingRequired([]byte(c.body), c.v); got != c.want {
			t.Errorf("%s into %T: %q, want %q", c.body, c.v, got, c.want)
		}
	}
}
