package mcpremote

import (
	"context"
	"encoding/json"
	"slices"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Allowlist is which of a server's tools one grant may call: those the server's admin switched
// on, narrowed to what the agent was given, and for a chat job only those the server marks
// read-only.
type Allowlist struct {
	Server   []string // mcp_servers.allowed_tools
	Agent    []string // empty: no narrowing
	ReadOnly bool
}

// AllowlistOf is a grant's.
func AllowlistOf(g Grant) Allowlist {
	return Allowlist{Server: g.Server.AllowedTools, Agent: g.Tools, ReadOnly: g.ReadOnly}
}

// Allows says whether t may be called.
func (a Allowlist) Allows(t *sdk.Tool) bool {
	if t == nil || !slices.Contains(a.Server, t.Name) {
		return false
	}
	if len(a.Agent) > 0 && !slices.Contains(a.Agent, t.Name) {
		return false
	}
	return !a.ReadOnly || (t.Annotations != nil && t.Annotations.ReadOnlyHint)
}

// allowlisted is an Upstream that offers only what the allowlist holds and refuses the rest,
// whatever a client asks for by name.
type allowlisted struct {
	Upstream
	allow Allowlist
}

func (u allowlisted) ListTools(ctx context.Context) ([]*sdk.Tool, error) {
	tools, err := u.Upstream.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(tools), func(t *sdk.Tool) bool { return !u.allow.Allows(t) }), nil
}

func (u allowlisted) CallTool(ctx context.Context, name string, args json.RawMessage) (*sdk.CallToolResult, error) {
	tools, err := u.Upstream.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(tools, func(t *sdk.Tool) bool { return t.Name == name })
	if i < 0 || !u.allow.Allows(tools[i]) {
		return nil, ErrDenied
	}
	return u.Upstream.CallTool(ctx, name, args)
}
