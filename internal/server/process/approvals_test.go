package process

import (
	"strings"
	"testing"
	"time"
)

// A project's approvals are the default until it says otherwise; an agent's own policy only adds
// to them and shortens the wait; a rule a runner cannot read, a wait it cannot keep and anything
// but a refusal on timeout are refused when the configuration is written.
func TestApprovalsPolicy(t *testing.T) {
	var c ProjectConfig
	if got := c.ApprovalsFor(nil); strings.Join(got.Ask, ",") != "Bash(curl:*),Bash(wget:*),Bash(git push:*)" || got.TimeoutOrDefault() != 30*time.Minute {
		t.Fatalf("default: %+v", got)
	}
	c.Policy = &Policy{Approvals: &Approvals{Ask: []string{}}}
	if got := c.ApprovalsFor(nil); len(got.Ask) != 0 {
		t.Fatalf("a project that asks nothing: %+v", got)
	}
	c.Policy.Approvals = &Approvals{Ask: []string{"WebFetch"}, Deny: []string{"Bash(rm -rf /*)"}, Timeout: "2h"}
	got := c.ApprovalsFor(&Approvals{Ask: []string{"mcp:sentry/*", "WebFetch"}, Deny: []string{"Bash(git push:*)"}, Timeout: "10m"})
	if strings.Join(got.Ask, ",") != "WebFetch,mcp:sentry/*" || strings.Join(got.Deny, ",") != "Bash(rm -rf /*),Bash(git push:*)" ||
		got.TimeoutOrDefault() != 10*time.Minute || got.OnTimeout != "deny" {
		t.Fatalf("tightened: %+v", got)
	}
	if got := c.ApprovalsFor(&Approvals{Timeout: "5h"}); got.TimeoutOrDefault() != 2*time.Hour {
		t.Fatalf("an agent lengthened the wait: %+v", got)
	}
	for _, bad := range []Approvals{{Ask: []string{"Bash("}}, {Ask: []string{"mcp:Sentry"}}, {Timeout: "10s"}, {Timeout: "48h"}, {OnTimeout: "allow"}} {
		if err := bad.Check(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := (Approvals{Ask: []string{"Bash(curl:*)", "mcp:sentry/*", "WebFetch(domain:example.com)"}, Timeout: "30m", OnTimeout: "deny"}).Check(); err != nil {
		t.Fatal(err)
	}
	if err := (ProjectConfig{Policy: &Policy{Approvals: &Approvals{OnTimeout: "allow"}}}).CheckBackends(); err == nil {
		t.Fatal("a configuration that allows on timeout was accepted")
	}
}
