package process

import "testing"

func TestPolicyForResolvesRoleOverDefault(t *testing.T) {
	c, err := ParseProjectConfig([]byte(`{"autopilot":{"backend":"codex"},"policy":{
		"default":{"backend":"claude","model":"opus","max_cost_usd":5},
		"roles":{"reviewer":{"model":"sonnet","max_cost_usd":1},"researcher":{"backend":"pi"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for role, want := range map[string]RolePolicy{
		"worker":     {Backend: "claude", Model: "opus", MaxCostUSD: 5},
		"reviewer":   {Backend: "claude", Model: "sonnet", MaxCostUSD: 1},
		"researcher": {Backend: "pi", MaxCostUSD: 5}, // another backend: the default model does not follow
	} {
		if got := c.PolicyFor(role, "server-default"); got != want {
			t.Errorf("%s: %+v, want %+v", role, got, want)
		}
	}
	bare, _ := ParseProjectConfig([]byte(`{"autopilot":{"backend":"codex"}}`))
	if got := bare.PolicyFor("worker", "claude"); got != (RolePolicy{Backend: "codex"}) {
		t.Errorf("without a policy the autopilot backend applies: %+v", got)
	}
	if got := (ProjectConfig{}).PolicyFor("worker", "claude"); got != (RolePolicy{Backend: "claude"}) {
		t.Errorf("empty config: %+v", got)
	}
}

// An agent's turn takes the role's policy, then its tier's (a cheaper model for chat), then the
// agent's own; a phase job ignores the tiers.
func TestPolicyForTurnLayersTierAndAgent(t *testing.T) {
	c, err := ParseProjectConfig([]byte(`{"policy":{
		"default":{"backend":"claude","model":"opus","max_cost_usd":5},
		"roles":{"reviewer":{"model":"sonnet","max_cost_usd":1}},
		"tiers":{"chat":{"model":"haiku","max_cost_usd":0.5}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, role, tier string
		own              RolePolicy
		want             RolePolicy
	}{
		{"a chat turn of the reviewer", "reviewer", "chat", RolePolicy{}, RolePolicy{Backend: "claude", Model: "haiku", MaxCostUSD: 0.5}},
		{"a work turn has no tier entry", "worker", "work", RolePolicy{}, RolePolicy{Backend: "claude", Model: "opus", MaxCostUSD: 5}},
		{"the agent's own cap wins", "reviewer", "chat", RolePolicy{MaxCostUSD: 2}, RolePolicy{Backend: "claude", Model: "haiku", MaxCostUSD: 2}},
		{"the agent's own backend drops the model", "reviewer", "chat", RolePolicy{Backend: "codex"}, RolePolicy{Backend: "codex", MaxCostUSD: 0.5}},
	}
	for _, tc := range cases {
		if got := c.PolicyForTurn(tc.role, tc.tier, tc.own, "claude"); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if got := c.PolicyFor("reviewer", "claude"); got.Model != "sonnet" {
		t.Errorf("a phase job ignores the tiers: %+v", got)
	}
}

// A role can run on an agent speaking ACP (acp:codex) as well as on Claude, per role, tier or
// agent; a name no runner could advertise is refused when the configuration is written.
func TestBackendNamesInAPolicy(t *testing.T) {
	cfg, err := ParseProjectConfig([]byte(`{"policy":{"default":{"backend":"claude"},
		"roles":{"worker":{"backend":"acp:codex","model":"gpt-5-codex"}},"tiers":{"chat":{"backend":"acp:gemini"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.CheckBackends(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.PolicyFor("worker", "claude"); got.Backend != "acp:codex" || got.Model != "gpt-5-codex" {
		t.Errorf("worker policy %+v", got)
	}
	if got := cfg.PolicyForTurn("reviewer", "chat", RolePolicy{}, "claude"); got.Backend != "acp:gemini" {
		t.Errorf("chat turn %+v", got)
	}
	for _, bad := range []string{`{"policy":{"roles":{"worker":{"backend":"acp:"}}}}`, `{"policy":{"default":{"backend":"Claude Code"}}}`,
		`{"autopilot":{"backend":"acp:codex:x"}}`, `{"policy":{"tiers":{"chat":{"backend":"acp:/bin/sh"}}}}`} {
		cfg, err := ParseProjectConfig([]byte(bad))
		if err == nil {
			err = cfg.CheckBackends()
		}
		if err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
