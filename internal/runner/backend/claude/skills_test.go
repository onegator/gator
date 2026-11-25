//go:build unix

package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
)

var goTesting = backend.Skill{Name: "go-testing", Description: "Write Go tests.", Files: []backend.SkillFile{
	{Path: "SKILL.md", Data: []byte("---\nname: go-testing\ndescription: Write Go tests.\n---\nTable tests.\n")},
	{Path: "scripts/run.sh", Data: []byte("#!/bin/sh\ngo test ./...\n")},
	{Path: "references/style.md", Data: []byte("No stutter.\n")},
}}

// The job's skills reach Claude Code as a plugin of their own, in the runner's directory and not
// the worktree, loaded for this session only with --plugin-dir; a read-only session keeps the
// Skill tool. The layout is the one `claude plugin validate` of 2.1.332 read as providing
// gator-skills:<name> (testdata/claude-2.1.332-plugin-validate.json).
func TestSkillsReachTheSessionAsAPluginOutsideTheWorktree(t *testing.T) {
	b, argsFile := fake(t, "claude-2.1.331-chat-readonly.ndjson")
	listing := filepath.Join(t.TempDir(), "plugin")
	t.Setenv("FAKE_CLAUDE_PLUGIN", listing)
	b.MCPDir = filepath.Join(t.TempDir(), "runner")
	work := t.TempDir()
	out, _ := run(t, b, context.Background(), backend.Spec{Dir: work, Prompt: "test it", Skills: []backend.Skill{goTesting},
		Access: backend.Access{ReadOnly: true, Commands: []string{"gator-cli"}}})
	if out.Status != proto.StatusDone {
		t.Fatalf("outcome: %+v", out)
	}
	args := argsOf(t, argsFile)
	dir := after(args, "--plugin-dir")
	if !strings.HasPrefix(dir, b.MCPDir+string(os.PathSeparator)) {
		t.Fatalf("the plugin is in the runner's directory: %q", dir)
	}
	if after(args, "--tools") != "Read,Grep,Glob,WebFetch,Skill,Bash" || !strings.Contains(after(args, "--allowedTools"), "Skill") {
		t.Fatalf("a read-only session reads skills: %q", args)
	}
	got, _ := os.ReadFile(listing)
	want := "600 ./.claude-plugin/plugin.json\n600 ./skills/go-testing/SKILL.md\n600 ./skills/go-testing/references/style.md\n700 ./skills/go-testing/scripts/run.sh\n"
	if string(got) != want {
		t.Fatalf("the plugin holds:\n%s\nwant:\n%s", got, want)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the plugin is removed when the run ends")
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Fatal("nothing of it is in the worktree")
	}

	var recorded struct {
		Success  bool `json:"success"`
		Provides struct {
			Skills []string `json:"skills"`
		} `json:"provides"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "claude-2.1.332-plugin-validate.json"))
	if err != nil || json.Unmarshal(raw, &recorded) != nil || !recorded.Success || len(recorded.Provides.Skills) != 1 ||
		recorded.Provides.Skills[0] != skillsPlugin+":go-testing" {
		t.Fatalf("the recording: %v %+v", err, recorded)
	}
}

// Without skills there is no plugin and no Skill tool; a skill whose file would leave its folder
// is not written at all.
func TestNoSkillsNoPluginAndUnsafePathsAreRefused(t *testing.T) {
	b, argsFile := fake(t, "claude-2.1.331-chat-readonly.ndjson")
	b.MCPDir = filepath.Join(t.TempDir(), "runner")
	run(t, b, context.Background(), backend.Spec{Prompt: "look", Access: backend.Access{ReadOnly: true}})
	args := argsOf(t, argsFile)
	if after(args, "--plugin-dir") != "" || strings.Contains(after(args, "--tools"), "Skill") {
		t.Fatalf("args: %q", args)
	}
	for _, p := range []string{"../../.ssh/authorized_keys", "/etc/passwd", "a/../../b", ""} {
		bad := backend.Skill{Name: "x", Files: []backend.SkillFile{{Path: p, Data: []byte("x")}}}
		out, _ := run(t, b, context.Background(), backend.Spec{Prompt: "look", Skills: []backend.Skill{bad}})
		if out.Status != proto.StatusFailed || !strings.Contains(out.Reason, "skills:") {
			t.Errorf("%q: %+v", p, out)
		}
	}
	if entries, _ := os.ReadDir(b.MCPDir); len(entries) != 0 {
		t.Fatalf("nothing is left behind: %v", entries)
	}
}
