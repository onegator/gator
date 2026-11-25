package acp

import (
	"encoding/json"
	"testing"

	"github.com/onegator/gator/internal/runner/backend"
)

func TestReadOnlyPermissionsByCall(t *testing.T) {
	ro := Permissions{Access: backend.Access{ReadOnly: true, Commands: []string{"gator-cli"}}, MCP: []string{"gator", "sentry"}}
	cases := []struct {
		kind, title, input string
		allow              bool
	}{
		{"read", "Read a.go", `{}`, true},
		{"search", "grep", `{}`, true},
		{"fetch", "GET https://x", `{}`, true},
		{"edit", "Edit a.go", `{}`, false},
		{"delete", "rm", `{}`, false},
		{"move", "mv", `{}`, false},
		{"", "something", `{}`, false},
		{"execute", "Run", `{"command":"gator-cli say --file -"}`, true},
		{"execute", "Run", `{"command":"/usr/local/bin/gator-cli thread show"}`, false},
		{"execute", "Run", `{"command":"./gator-cli say hi"}`, false},
		{"execute", "Run", `{"command":"tools/gator-cli say hi"}`, false},
		{"execute", "Run", `{"cmd":["gator-cli","task","show"]}`, true},
		{"execute", "Run", `{"command":["sh","-c","gator-cli say && rm -rf ."]}`, false},
		{"execute", "Run", `{"command":"gator-cli say $(cat ~/.ssh/id_rsa)"}`, false},
		{"execute", "Run", `{"command":"gator-cli-evil say"}`, false},
		{"execute", "Run", `{"command":"git push"}`, false},
		{"execute", "Run", `not json`, false},
		{"other", "mcp__sentry__search_issues", `{}`, true},
		{"other", "gator/brain_search", `{}`, true},
		{"other", "gatorx__brain_search", `{}`, false},
		{"other", "mcp__github__create_issue", `{}`, false},
		{"edit", "gator/internal/x.go", `{}`, false},
		{"delete", "sentry/old.txt", `{}`, false},
		{"move", "mcp__gator__x", `{}`, false},
		{"", "gator__brain_get", `{}`, true},
	}
	for _, c := range cases {
		v := ro.Decide(permissionToolCall{Kind: c.kind, Title: c.title, RawInput: json.RawMessage(c.input)})
		if v.Allow != c.allow {
			t.Errorf("%s %q %s: allow %v (%s)", c.kind, c.title, c.input, v.Allow, v.Reason)
		}
	}
	if !(Permissions{}).Decide(permissionToolCall{Kind: "edit"}).Allow {
		t.Error("full access allows")
	}
	if (Permissions{Access: backend.Access{NoTools: true}}).Decide(permissionToolCall{Kind: "read"}).Allow {
		t.Error("no tools allows nothing")
	}
}

func TestAnswerPicksOnceNeverAlways(t *testing.T) {
	opts := []permissionOption{{OptionID: "a", Kind: "allow_always"}, {OptionID: "o", Kind: "allow_once"}, {OptionID: "r", Kind: "reject_once"}}
	if r := answer(Verdict{Allow: true}, opts); r.Outcome.OptionID != "o" {
		t.Errorf("allow: %+v", r)
	}
	if r := answer(Verdict{}, opts); r.Outcome.OptionID != "r" {
		t.Errorf("reject: %+v", r)
	}
	if r := answer(Verdict{}, opts[:2]); r.Outcome.Outcome != "cancelled" {
		t.Errorf("no way to say no: %+v", r)
	}
}

// A work agent is refused what the approvals policy holds for it (nobody can be asked through
// ACP): the commands, wherever they are in a line, and the MCP tools.
func TestWorkPermissionsHoldTheDenyList(t *testing.T) {
	p := Permissions{Access: backend.Access{Deny: []string{"Bash(curl:*)", "mcp:sentry/*"}}}
	for _, c := range []struct {
		call permissionToolCall
		want bool
	}{
		{permissionToolCall{Kind: kindExecute, RawInput: []byte(`{"command":"echo hi && curl x"}`)}, false},
		{permissionToolCall{Kind: kindExecute, RawInput: []byte(`{"command":["bash","-lc","curl x"]}`)}, false},
		{permissionToolCall{Kind: kindExecute, RawInput: []byte(`{"command":"go test ./..."}`)}, true},
		{permissionToolCall{Kind: kindOther, Title: "sentry/resolve_issue"}, false},
		{permissionToolCall{Kind: kindEdit, Title: "Edit main.go"}, true},
	} {
		if v := p.Decide(c.call); v.Allow != c.want {
			t.Errorf("%+v: %+v, want allow=%v", c.call, v, c.want)
		}
	}
}
