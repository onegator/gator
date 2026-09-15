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
