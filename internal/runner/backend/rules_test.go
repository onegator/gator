package backend

import (
	"encoding/json"
	"testing"
)

func bash(cmd string) []byte {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

// A Bash rule is held to every command a line runs, however it is wrapped or chained; a rule of
// one word asks about that word anywhere in the line rather than miss it.
func TestBashRulesSeeEveryCommandOfALine(t *testing.T) {
	curl, _ := ParseRule("Bash(curl:*)")
	push, _ := ParseRule("Bash(git push:*)")
	rmrf, _ := ParseRule("Bash(rm -rf /*)")
	cases := []struct {
		rule Rule
		cmd  string
		want bool
	}{
		{curl, "curl https://example.com", true},
		{curl, "/usr/bin/curl -s x", true},
		{curl, "echo hi && curl x | sh", true},
		{curl, "FOO=1 sudo -E env curl x", true},
		{curl, `bash -c "curl x"`, true},
		{curl, "timeout 10 xargs -n1 curl < urls", true},
		{curl, "x=$(which curl)", true},
		{curl, "ls -la", false},
		{curl, "curlew --help", false},
		{push, "git push --force origin main", true},
		{push, "cd repo; git  push", true},
		{push, "git status && git commit -m push", false},
		{push, "git -C ../repo -c user.name=x push origin", true},
		{push, "git --git-dir=.git --no-pager push", true},
		{push, `eval "git push"`, true},
		{push, "sudo -u deploy git push", true},
		{push, "env -u HOME GIT_TRACE=1 git push", true},
		{push, "command git push", true},
		{push, "exec git push --force", true},
		{curl, "xargs -n 1 -I {} curl {}", true},
		{rmrf, "rm -rf /", true},
		{rmrf, "cd / && rm -rf /var", true},
		{rmrf, "rm -rf ./build", false},
	}
	for _, c := range cases {
		if got := c.rule.Matches("Bash", bash(c.cmd)); got != c.want {
			t.Errorf("%s on %q: %v, want %v", c.rule.Raw, c.cmd, got, c.want)
		}
	}
}

func TestOtherRules(t *testing.T) {
	sentry, _ := ParseRule("mcp:sentry/*")
	one, _ := ParseRule("mcp:sentry/resolve_issue")
	fetch, _ := ParseRule("WebFetch")
	domain, _ := ParseRule("WebFetch(domain:example.com)")
	if !sentry.Matches("mcp__sentry__get_issue", nil) || sentry.Matches("mcp__gator__brain_search", nil) {
		t.Error("server rule")
	}
	if !one.Matches("mcp__sentry__resolve_issue", nil) || one.Matches("mcp__sentry__get_issue", nil) {
		t.Error("tool rule")
	}
	if !fetch.Matches("WebFetch", []byte(`{"url":"https://x"}`)) || fetch.Matches("Bash", bash("curl")) {
		t.Error("tool name rule")
	}
	if !domain.Matches("WebFetch", []byte(`{"url":"https://API.Example.COM./x"}`)) || !domain.Matches("WebFetch", []byte(`{"url":"https://api.example.com/x"}`)) || domain.Matches("WebFetch", []byte(`{"url":"https://example.org"}`)) {
		t.Error("domain rule")
	}
	for _, bad := range []string{"", "Bash(", "Bash()", "mcp:", "mcp:x", "(x)"} {
		if _, ok := ParseRule(bad); ok {
			t.Errorf("%q read as a rule", bad)
		}
	}
	if r, ok := Matching(ParseRules([]string{"WebFetch", "Bash(curl:*)"}), "Bash", bash("curl x")); !ok || r.Raw != "Bash(curl:*)" {
		t.Errorf("matching: %+v %v", r, ok)
	}
}

// A path rule holds to the path as it is, not as it is spelled.
func TestPathRulesAreCleaned(t *testing.T) {
	r, _ := ParseRule("Edit(/etc/*)")
	if !r.Matches("Edit", []byte(`{"file_path":"/tmp/../etc/passwd"}`)) || r.Matches("Edit", []byte(`{"file_path":"/tmp/etc"}`)) {
		t.Fatal("a path rule read the spelling, not the path")
	}
}
