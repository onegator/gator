// Package names holds the names the marketplace's pieces share — a package, a skill, a remote MCP
// server, one of its tools — so the server that stores them and the runner that puts them in a
// path or a tool's name check them by one rule.
package names

import (
	"regexp"
	"strings"
)

var (
	name = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	tool = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
)

// Valid says whether s can name a package, a registry, a skill or a remote MCP server: lowercase
// letters, digits and dashes, starting with a letter, at most 63 long.
func Valid(s string) bool { return name.MatchString(s) }

// ValidTool says whether s can name a remote MCP tool.
func ValidTool(s string) bool { return tool.MatchString(s) }

// ValidGrant says whether s can be one of an agent's tools: a server's name, or server/tool.
func ValidGrant(s string) bool {
	server, t, one := strings.Cut(s, "/")
	return Valid(server) && (!one || ValidTool(t))
}
