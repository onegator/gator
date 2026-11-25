package executor

import (
	"fmt"
	"strings"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
)

// skillsOf are the job's skills as a backend takes them.
func skillsOf(job proto.Job) []backend.Skill {
	out := make([]backend.Skill, 0, len(job.Skills))
	for _, sk := range job.Skills {
		s := backend.Skill{Name: sk.Name, Description: sk.Description}
		for _, f := range sk.Files {
			s.Files = append(s.Files, backend.SkillFile{Path: f.Path, Data: f.Data})
		}
		out = append(out, s)
	}
	return out
}

// SkillsPrompt is how a backend that does not load skills by itself (the instructions mode) learns
// of them: each skill's name and description, read whole through Gator's skill_read when it
// applies. A job with no MCP to read them through gets each SKILL.md in the prompt instead.
func SkillsPrompt(skills []proto.Skill, canRead bool) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	if canRead {
		b.WriteString("Skills you have. Each is a folder of instructions, sometimes with scripts and references. When one applies, read it first with Gator's MCP tool skill_read (mcp__" + MCPServerName + "__skill_read, {\"name\": \"<skill>\"}; a file of it with \"path\") and follow it:\n\n")
		for _, sk := range skills {
			fmt.Fprintf(&b, "- %s: %s\n", sk.Name, oneLine(sk.Description))
		}
		return b.String()
	}
	b.WriteString("Skills you have. When one applies, follow it:\n")
	for _, sk := range skills {
		for _, f := range sk.Files {
			if f.Path == "SKILL.md" {
				fmt.Fprintf(&b, "\n## Skill: %s\n\n%s\n", sk.Name, strings.TrimSpace(string(f.Data)))
			}
		}
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
