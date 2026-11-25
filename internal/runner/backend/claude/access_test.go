//go:build unix

package claude

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
)

// argsOf reads the arguments the fake was called with, one per line.
func argsOf(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n---\n"), "\n")
}

func after(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// A read-only session runs in dontAsk mode with only the tools that read, Bash only for the
// named commands, and the writing tools denied by name. The recording is of Claude Code 2.1.331
// run with exactly these flags: Write was not there to call, a `touch` through Bash was denied,
// and the session still ended with an answer.
func TestReadOnlyAccessReachesTheCLI(t *testing.T) {
	b, argsFile := fake(t, "claude-2.1.331-chat-readonly.ndjson")
	out, evs := run(t, b, context.Background(), backend.Spec{Prompt: "look", Access: backend.Access{ReadOnly: true, Commands: []string{"gator-cli"}}})
	args := argsOf(t, argsFile)
	if after(args, "--permission-mode") != "dontAsk" || after(args, "--tools") != "Read,Grep,Glob,WebFetch,Bash" ||
		after(args, "--allowedTools") != "Read,Grep,Glob,WebFetch,Bash(gator-cli:*)" || after(args, "--disallowedTools") != "Edit,Write,NotebookEdit" {
		t.Fatalf("args: %q", args)
	}
	for _, a := range args {
		if a == "bypassPermissions" {
			t.Fatal("a read-only session must not bypass permissions")
		}
	}
	if out.Status != proto.StatusDone || !strings.Contains(out.Summary, "disabled") || !strings.Contains(out.Summary, "denied") || out.Usage.CostUSD <= 0 {
		t.Fatalf("outcome: %+v", out)
	}
	if !strings.Contains(strings.Join(evs, ","), "tool_call,tool_result") {
		t.Fatalf("events: %v", evs)
	}

	// Without commands to run there is no Bash at all.
	b, argsFile = fake(t, "claude-2.1.331-chat-readonly.ndjson")
	run(t, b, context.Background(), backend.Spec{Prompt: "look", Access: backend.Access{ReadOnly: true}})
	args = argsOf(t, argsFile)
	if after(args, "--tools") != "Read,Grep,Glob,WebFetch" || strings.Contains(after(args, "--allowedTools"), "Bash") {
		t.Fatalf("args: %q", args)
	}
}

// What the allow-list lets through: a recorded session that ran gator-cli in dontAsk mode.
func TestReadOnlyRunsGatorCLI(t *testing.T) {
	b, _ := fake(t, "claude-2.1.331-chat-gator-cli.ndjson")
	out, evs := run(t, b, context.Background(), backend.Spec{Prompt: "talk", Access: backend.Access{ReadOnly: true, Commands: []string{"gator-cli"}}})
	if out.Status != proto.StatusDone || !strings.Contains(out.Summary, `"ok":true`) || out.ToolCalls != 1 {
		t.Fatalf("outcome: %+v (%v)", out, evs)
	}
}

// Full access is what every job had before tiers.
func TestFullAccessBypassesPermissions(t *testing.T) {
	b, argsFile := fake(t, "claude-2.1.270-tool-success.ndjson")
	run(t, b, context.Background(), backend.Spec{Prompt: "work"})
	args := argsOf(t, argsFile)
	if after(args, "--permission-mode") != "bypassPermissions" || after(args, "--tools") != "" || after(args, "--disallowedTools") != "" {
		t.Fatalf("args: %q", args)
	}
}

// A session with no tools at all gets none of the built-in ones.
func TestNoToolsReachesTheCLI(t *testing.T) {
	b, argsFile := fake(t, "claude-2.1.331-chat-readonly.ndjson")
	run(t, b, context.Background(), backend.Spec{Prompt: "sum up", Access: backend.Access{ReadOnly: true, NoTools: true}})
	args := argsOf(t, argsFile)
	if after(args, "--permission-mode") != "dontAsk" || after(args, "--allowedTools") != "" {
		t.Fatalf("args: %q", args)
	}
	found := false
	for i, a := range args {
		if a == "--tools" && i+1 < len(args) && args[i+1] == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no --tools \"\": %q", args)
	}
}

// What the approvals policy denies is denied by the CLI itself, in every mode.
func TestDeniedToolsAreDisallowed(t *testing.T) {
	got := strings.Join(permissionArgs(backend.Access{Deny: []string{"Bash(curl:*)", "WebFetch", "mcp:sentry/*", "mcp:linear/create_issue"}}, nil, false), "\n")
	if got != "--permission-mode\nbypassPermissions\n--disallowedTools\nBash(curl:*),WebFetch,mcp__sentry,mcp__linear__create_issue" {
		t.Fatalf("args:\n%s", got)
	}
}
