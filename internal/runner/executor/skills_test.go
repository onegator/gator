package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/workspace"
)

// loader is a backend that reads skills by itself.
type loader struct{ scripted }

func (*loader) LoadsSkills() bool { return true }

var skilled = proto.Job{JobID: "j", Backend: "fake", AgentToken: "gtr_job", Skills: []proto.Skill{{Name: "go-testing", Description: "Write Go\n tests.",
	Files: []proto.SkillFile{{Path: "SKILL.md", Data: []byte("---\nname: go-testing\n---\nTable tests.")}, {Path: "scripts/run.sh", Data: []byte("go test")}}}}}

func done(context.Context, int, backend.Spec, backend.Emit) backend.Outcome {
	return backend.Outcome{Status: proto.StatusDone, Summary: digestBlock, SessionID: "s", Usage: usage()}
}

// A backend that loads skills gets them whole and its prompt says nothing of them (native mode).
// Any other is told each skill's name and description and reads one through skill_read
// (instructions mode); with no MCP to read through, the SKILL.md itself is in the prompt.
func TestSkillsReachEveryBackendOneWayOrAnother(t *testing.T) {
	native := &loader{scripted{run: done}}
	ex := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": native}, ServerURL: "http://gator"}
	io, _, _, _ := jobIO()
	ex.Run(context.Background(), skilled, io)
	if s := native.specs[0]; len(s.Skills) != 1 || len(s.Skills[0].Files) != 2 || strings.Contains(s.Prompt, "skill_read") {
		t.Fatalf("native: %+v", s.Skills)
	}

	plain := &scripted{run: done}
	ex.Backends = map[string]backend.Backend{"fake": plain}
	io, _, _, _ = jobIO()
	ex.Run(context.Background(), skilled, io)
	s := plain.specs[0]
	if len(s.Skills) != 0 || !strings.Contains(s.Prompt, "mcp__gator__skill_read") || !strings.Contains(s.Prompt, "- go-testing: Write Go tests.") ||
		strings.Contains(s.Prompt, "Table tests.") {
		t.Fatalf("instructions: %d skills, prompt:\n%s", len(s.Skills), s.Prompt)
	}

	alone := &scripted{run: done}
	ex = &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": alone}}
	io, _, _, _ = jobIO()
	ex.Run(context.Background(), skilled, io)
	if p := alone.specs[0].Prompt; strings.Contains(p, "skill_read") || !strings.Contains(p, "## Skill: go-testing") || !strings.Contains(p, "Table tests.") {
		t.Fatalf("no MCP:\n%s", p)
	}
}
