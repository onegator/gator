//go:build unix

package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/onegator/gator/internal/runner/backend"
)

// mcpConfig is what --mcp-config gets: the runner's own configuration (a JSON string or a file,
// "explicit or empty") with the job's servers added. A job's servers carry its token, so they go
// into a file only this user reads, outside the worktree, removed when the run ends — never onto
// the command line, where any process on the machine could read it.
func mcpConfig(base string, servers []backend.MCPServer) (string, func(), error) {
	nothing := func() {}
	if len(servers) == 0 {
		if base == "" {
			return noMCP, nothing, nil
		}
		return base, nothing, nil
	}
	cfg, err := readMCP(base)
	if err != nil {
		return "", nothing, err
	}
	for _, s := range servers {
		entry := map[string]any{"type": "http", "url": s.URL}
		if len(s.Headers) > 0 {
			entry["headers"] = s.Headers
		}
		cfg[s.Name] = entry // the job's own server wins over a runner's of the same name
	}
	b, err := json.Marshal(map[string]any{"mcpServers": cfg})
	if err != nil {
		return "", nothing, err
	}
	f, err := os.CreateTemp("", "gator-mcp-*.json")
	if err != nil {
		return "", nothing, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", nothing, err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		cleanup()
		return "", nothing, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nothing, err
	}
	return f.Name(), cleanup, nil
}

// readMCP reads the runner's own servers from a JSON string or a file.
func readMCP(base string) (map[string]any, error) {
	if strings.TrimSpace(base) == "" {
		return map[string]any{}, nil
	}
	raw := []byte(base)
	if !strings.HasPrefix(strings.TrimSpace(base), "{") {
		b, err := os.ReadFile(base)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	var cfg struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("the runner's MCP configuration is not JSON: %w", err)
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]any{}
	}
	return cfg.MCPServers, nil
}

func mcpNames(servers []backend.MCPServer) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Name)
	}
	return out
}
