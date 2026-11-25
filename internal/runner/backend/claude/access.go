package claude

import (
	"strings"

	"github.com/onegator/gator/internal/runner/backend"
)

// Claude Code's built-in tools a read-only session keeps (checked against `claude --help` of
// 2.1.331 and recorded in testdata/claude-2.1.331-chat-*.ndjson): it reads, searches and
// fetches. Bash stays available only so that the commands Access names can run.
var readOnlyTools = []string{"Read", "Grep", "Glob", "WebFetch"}

// writingTools are the built-in tools that change files; a read-only session is denied them by
// name as well as by leaving them out, so a later CLI that adds them back by default still
// cannot.
var writingTools = []string{"Edit", "Write", "NotebookEdit"}

// skillTool is Claude Code's tool that reads a skill when it applies.
const skillTool = "Skill"

// Supports implements backend.Limited: Claude Code keeps read-only and no-tools sessions with the
// flags below.
func (Backend) Supports(backend.Access) bool { return true }

// permissionArgs are the flags that give a session the access asked for. Full access is
// today's job: bypassPermissions, since nobody is there to answer a prompt. Read-only is
// dontAsk — anything not allowed up front is denied, never asked about — with only the reading
// tools, Bash limited to the named commands, and the writing tools denied. No tools is dontAsk
// with an empty --tools: no built-in tool at all.
//
// The MCP servers named (Gator's, and the remote ones the agent was given through Gator's proxy)
// are allowed whole in a read-only session: their tools are the job's own identity, held to its
// kind and its agent's grant by the server. A session with skills keeps the Skill tool, which
// reads a skill; its scripts still run only where Bash may.
func permissionArgs(a backend.Access, mcp []string, skills bool) []string {
	if a.NoTools {
		return []string{"--permission-mode", "dontAsk", "--tools", "", "--disallowedTools", strings.Join(writingTools, ",")}
	}
	if !a.ReadOnly {
		args := []string{"--permission-mode", "bypassPermissions"}
		if len(a.Deny) > 0 {
			// Deny rules hold in every permission mode, bypass included.
			args = append(args, "--disallowedTools", strings.Join(a.Deny, ","))
		}
		return args
	}
	tools := append([]string{}, readOnlyTools...)
	allowed := append([]string{}, readOnlyTools...)
	if skills {
		tools, allowed = append(tools, skillTool), append(allowed, skillTool)
	}
	for _, name := range mcp {
		allowed = append(allowed, "mcp__"+name)
	}
	if len(a.Commands) > 0 {
		tools = append(tools, "Bash")
		for _, c := range a.Commands {
			allowed = append(allowed, "Bash("+c+":*)")
		}
	}
	return []string{"--permission-mode", "dontAsk",
		"--tools", strings.Join(tools, ","),
		"--allowedTools", strings.Join(allowed, ","),
		"--disallowedTools", strings.Join(writingTools, ",")}
}
