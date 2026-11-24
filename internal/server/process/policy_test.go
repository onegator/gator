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
