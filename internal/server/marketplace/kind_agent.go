package marketplace

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// AgentFile is an agent package's manifest.
const AgentFile = "agent.yaml"

// AgentManifest is agent.yaml: a member of the team with a role, the skills it brings, what it
// needs of the project and what it asks to be allowed.
type AgentManifest struct {
	Name          string         `yaml:"name" json:"name"`
	Version       string         `yaml:"version" json:"version"`
	DisplayName   string         `yaml:"display_name" json:"display_name"`
	Handle        string         `yaml:"handle" json:"handle"` // suggested; an admin picks another when it is taken
	Description   string         `yaml:"description" json:"description"`
	Avatar        string         `yaml:"avatar,omitempty" json:"avatar,omitempty"`
	Icon          string         `yaml:"icon,omitempty" json:"icon,omitempty"`
	Phases        []string       `yaml:"phases,omitempty" json:"phases,omitempty"`
	RoleKey       string         `yaml:"role_key,omitempty" json:"role_key,omitempty"`
	Tier          string         `yaml:"tier" json:"tier"`
	Policy        AgentPolicy    `yaml:"policy,omitempty" json:"policy"`
	Skills        []string       `yaml:"skills,omitempty" json:"skills,omitempty"`
	Tools         []string       `yaml:"tools,omitempty" json:"tools,omitempty"` // MCP servers, or server/tool
	Requires      AgentRequires  `yaml:"requires,omitempty" json:"requires"`
	Triggers      []AgentTrigger `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	Permissions   []string       `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	Instructions  string         `yaml:"instructions,omitempty" json:"instructions_file,omitempty"` // default instructions.md
	ExamplePrompt string         `yaml:"example_prompt,omitempty" json:"example_prompt,omitempty"`
}

// AgentPolicy is the agent's own backend, model and spending.
type AgentPolicy struct {
	Backend        string  `yaml:"backend,omitempty" json:"backend,omitempty"`
	Model          string  `yaml:"model,omitempty" json:"model,omitempty"`
	MaxCostUSD     float64 `yaml:"max_cost_usd,omitempty" json:"max_cost_usd,omitempty"`
	DailyBudgetUSD float64 `yaml:"daily_budget_usd,omitempty" json:"daily_budget_usd,omitempty"`
}

// AgentRequires is what the agent needs of its project.
type AgentRequires struct {
	Capabilities []string `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	MCP          []string `yaml:"mcp,omitempty" json:"mcp,omitempty"`
}

// AgentTrigger is an automation the agent proposes; a person switches each one on.
type AgentTrigger struct {
	On   string         `yaml:"on" json:"on"`
	When map[string]any `yaml:"when,omitempty" json:"when,omitempty"`
}

var handlePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// ValidHandle says whether s can be typed after @.
func ValidHandle(s string) bool { return handlePattern.MatchString(s) }

// Agent is the agent kind.
type Agent struct{}

// Kind implements Kind.
func (Agent) Kind() string { return KindAgent }

// Describe implements Kind.
func (Agent) Describe(_ Item, files Files) (Manifest, error) {
	am, _, err := ParseAgent(files)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{Name: am.Name, Version: am.Version, DisplayName: am.DisplayName, Description: am.Description,
		Phases: am.Phases, Icon: am.Icon, ExamplePrompt: am.ExamplePrompt, Detail: detail(am),
		Requires: Requires{Capabilities: am.Requires.Capabilities, MCP: am.Requires.MCP}}
	if m.Icon == "" {
		m.Icon = am.Avatar
	}
	m.Permissions = append(m.Permissions, am.Permissions...)
	m.Permissions = append(m.Permissions, agentPermissions(am)...)
	for _, name := range am.Skills {
		bundled := files.Under("skills/" + name)
		if len(bundled) == 0 {
			m.Requires.Skills = append(m.Requires.Skills, name)
			continue
		}
		sm, err := describeSkill(bundled)
		if err != nil {
			return m, fmt.Errorf("skill %s: %w", name, err)
		}
		if sm.Name != name {
			return m, invalid("skills/%s names itself %s", name, sm.Name)
		}
		m.Permissions = append(m.Permissions, sm.Permissions...)
	}
	return m, nil
}

// ParseAgent reads an agent package's manifest and its instructions.
func ParseAgent(files Files) (AgentManifest, string, error) {
	var am AgentManifest
	if err := readYAML(files, AgentFile, &am); err != nil {
		return am, "", err
	}
	for field, v := range map[string]string{"name": am.Name, "version": am.Version, "display_name": am.DisplayName, "description": am.Description} {
		if err := requireText(field, v); err != nil {
			return am, "", err
		}
	}
	if !ValidHandle(am.Handle) {
		return am, "", invalid("handle %q must be 2–32 of a–z, 0–9 and '-'", am.Handle)
	}
	if am.Tier != "chat" && am.Tier != "work" {
		return am, "", invalid("tier must be chat or work, not %q", am.Tier)
	}
	if am.Policy.MaxCostUSD < 0 || am.Policy.DailyBudgetUSD < 0 {
		return am, "", invalid("a policy does not spend less than nothing")
	}
	for _, s := range am.Skills {
		if !ValidName(s) {
			return am, "", invalid("skill %q is not a name", s)
		}
	}
	for _, t := range append(slices.Clone(am.Tools), am.Requires.MCP...) {
		server, tool, _ := strings.Cut(t, "/")
		if !ValidName(server) || (tool != "" && strings.TrimSpace(tool) == "") {
			return am, "", invalid("tool %q must be an MCP server's name, or server/tool", t)
		}
	}
	for _, tr := range am.Triggers {
		if err := requireText("triggers.on", tr.On); err != nil {
			return am, "", err
		}
	}
	file := am.Instructions
	if file == "" {
		file = "instructions.md"
	}
	instructions, err := textFile(files, file)
	if err != nil {
		return am, "", err
	}
	if strings.TrimSpace(instructions) == "" {
		return am, "", invalid("%s is empty", file)
	}
	return am, instructions, nil
}

// agentPermissions spell out what an agent's manifest lets it do beyond what it says itself.
func agentPermissions(am AgentManifest) []string {
	var out []string
	if am.Tier == "work" {
		out = append(out, "works on code: commits on its own gator/ branch of the project's repository, on tasks where a person allowed agents")
	}
	if am.Policy.MaxCostUSD > 0 {
		out = append(out, fmt.Sprintf("spends up to $%.2f a turn", am.Policy.MaxCostUSD))
	}
	if am.Policy.DailyBudgetUSD > 0 {
		out = append(out, fmt.Sprintf("spends up to $%.2f a day", am.Policy.DailyBudgetUSD))
	}
	if am.RoleKey != "" {
		out = append(out, "may play the process role "+am.RoleKey)
	}
	for _, c := range am.Requires.Capabilities {
		out = append(out, "needs the capability "+c)
	}
	for _, s := range am.Requires.MCP {
		out = append(out, "uses the MCP server "+s)
	}
	for _, t := range am.Tools {
		out = append(out, "calls MCP tools of "+t)
	}
	for _, tr := range am.Triggers {
		when, _ := json.Marshal(tr.When)
		if len(tr.When) == 0 {
			when = nil
		}
		out = append(out, strings.TrimSpace("proposes an automation on "+tr.On+" "+string(when)))
	}
	return out
}
