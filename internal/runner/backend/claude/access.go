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

// permissionArgs are the flags that give a session the access asked for. Full access is
// today's job: bypassPermissions, since nobody is there to answer a prompt. Read-only is
// dontAsk — anything not allowed up front is denied, never asked about — with only the reading
// tools, Bash limited to the named commands, and the writing tools denied.
func permissionArgs(a backend.Access) []string {
	if !a.ReadOnly {
		return []string{"--permission-mode", "bypassPermissions"}
	}
	tools := append([]string{}, readOnlyTools...)
	allowed := append([]string{}, readOnlyTools...)
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
