package process

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Approvals is process_config.policy.approvals: the tools of a work turn that wait for a person
// (ask), those never allowed (deny), and how long a request waits before it is refused. Everything
// else a work agent may do in its worktree, as before. An agent's own policy can only tighten it.
//
//	{"approvals": {"ask": ["Bash(curl:*)", "Bash(git push:*)", "mcp:sentry/*", "WebFetch"],
//	               "deny": ["Bash(rm -rf /*)"], "timeout": "30m", "onTimeout": "deny"}}
type Approvals struct {
	Ask     []string `json:"ask"`
	Deny    []string `json:"deny,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
	// OnTimeout is what an unanswered request comes to: "deny", the only answer a request nobody
	// gave can have. Nothing is ever allowed because nobody said no.
	OnTimeout string `json:"onTimeout,omitempty"`
}

// DefaultApprovals is what a project that says nothing asks about: short on purpose — the network
// and pushing, which an agent working in its worktree does not need and a person wants to see.
var DefaultApprovals = Approvals{Ask: []string{"Bash(curl:*)", "Bash(wget:*)", "Bash(git push:*)"}, Timeout: "30m", OnTimeout: "deny"}

// Bounds of a request's wait.
const (
	DefaultApprovalTimeout = 30 * time.Minute
	MaxApprovalTimeout     = 24 * time.Hour
	minApprovalTimeout     = time.Minute
	maxApprovalRules       = 100
)

// approvalRule is a tool (Bash, WebFetch, mcp__server__tool) with an optional argument pattern in
// parentheses, or mcp:<server>/<tool> with * for any tool.
var approvalRule = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9_]{0,63}(?:\(.{1,200}\))?|mcp:[a-z0-9][a-z0-9_-]{0,63}/(?:\*|[A-Za-z0-9_.-]{1,128}))$`)

// Check refuses approvals a runner could not hold: a rule it cannot read, a timeout it cannot
// keep, anything but deny on timeout.
func (a Approvals) Check() error {
	if len(a.Ask)+len(a.Deny) > maxApprovalRules {
		return fmt.Errorf("process_config: policy.approvals has more than %d rules", maxApprovalRules)
	}
	for _, r := range append(append([]string{}, a.Ask...), a.Deny...) {
		if !approvalRule.MatchString(r) {
			return fmt.Errorf("process_config: policy.approvals rule %q is not Tool, Tool(pattern) or mcp:server/tool", r)
		}
	}
	if a.Timeout != "" {
		d, err := time.ParseDuration(a.Timeout)
		if err != nil || d < minApprovalTimeout || d > MaxApprovalTimeout {
			return fmt.Errorf("process_config: policy.approvals.timeout %q is a duration from 1m to 24h", a.Timeout)
		}
	}
	if a.OnTimeout != "" && a.OnTimeout != "deny" {
		return fmt.Errorf("process_config: policy.approvals.onTimeout is deny; a request nobody answered is never allowed")
	}
	return nil
}

// TimeoutOrDefault is how long a request waits.
func (a Approvals) TimeoutOrDefault() time.Duration {
	if d, err := time.ParseDuration(a.Timeout); err == nil && d >= minApprovalTimeout && d <= MaxApprovalTimeout {
		return d
	}
	return DefaultApprovalTimeout
}

// Tighten is a with what own adds: more to ask about and more to deny, and the shorter wait. An
// agent's policy cannot take a rule of the project's away.
func (a Approvals) Tighten(own *Approvals) Approvals {
	out := Approvals{Ask: slices.Clone(a.Ask), Deny: slices.Clone(a.Deny), Timeout: a.Timeout, OnTimeout: "deny"}
	if own == nil {
		return out
	}
	for _, r := range own.Ask {
		if !slices.Contains(out.Ask, r) {
			out.Ask = append(out.Ask, r)
		}
	}
	for _, r := range own.Deny {
		if !slices.Contains(out.Deny, r) {
			out.Deny = append(out.Deny, r)
		}
	}
	if own.Timeout != "" && own.TimeoutOrDefault() < a.TimeoutOrDefault() {
		out.Timeout = own.Timeout
	}
	return out
}

// ApprovalsFor is the approvals of a project's work turn by an agent with its own policy: the
// project's (or the default), tightened by the agent's.
func (c ProjectConfig) ApprovalsFor(own *Approvals) Approvals {
	base := DefaultApprovals
	if c.Policy != nil && c.Policy.Approvals != nil {
		base = *c.Policy.Approvals
	}
	return base.Tighten(own)
}

// JobApprovals is ApprovalsFor for a project and an agent's policy (agents.policy, as JSON).
func (s *Service) JobApprovals(ctx context.Context, projectID pgtype.UUID, agentPolicy []byte) (Approvals, error) {
	c, err := s.projectConfig(ctx, projectID)
	if err != nil {
		return Approvals{}, err
	}
	var own RolePolicy
	if len(agentPolicy) > 0 {
		_ = json.Unmarshal(agentPolicy, &own)
	}
	return c.ApprovalsFor(own.Approvals), nil
}
