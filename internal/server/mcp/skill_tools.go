package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/jobscope"
	"github.com/onegator/gator/internal/server/store/db"
)

// skills reads the skills a job's agent carries. *db.Queries is one.
type skills interface {
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	SkillsNamed(ctx context.Context, arg db.SkillsNamedParams) ([]db.Skill, error)
}

// SkillTools are for a backend that does not load skills by itself (the instructions mode): the
// prompt names the skills, and the agent reads one here. Only the skills the job's own agent
// carries are there to read.
func SkillTools(st skills) []Tool {
	return []Tool{
		tool[skillIn, skillOut]{name: "skill_read", who: access{command: jobscope.SkillRead},
			about: "Read a skill you carry: its SKILL.md (the default), or one of its files by path. Follow it when it applies.",
			call: func(ctx context.Context, c Caller, in skillIn) (skillOut, map[string]any, error) {
				fields := map[string]any{"skill": in.Name, "path": in.Path}
				sk, err := carried(ctx, st, c.Job, in.Name)
				if err != nil {
					return skillOut{}, fields, err
				}
				var files []struct {
					Path string `json:"path"`
					Data []byte `json:"data"`
				}
				if err := json.Unmarshal(sk.Files, &files); err != nil {
					return skillOut{}, fields, err
				}
				want := in.Path
				if want == "" {
					want = "SKILL.md"
				}
				out := skillOut{Name: sk.Name, Version: sk.Version, Path: want}
				found := false
				for _, f := range files {
					out.Files = append(out.Files, f.Path)
					if f.Path == want {
						found = true
						if bytes.IndexByte(f.Data, 0) >= 0 {
							return skillOut{}, fields, refusal(want + " is not text; it is there for scripts that use it")
						}
						out.Content = string(f.Data)
					}
				}
				if !found {
					return skillOut{}, fields, refusal(want + " is not a file of " + sk.Name)
				}
				return out, fields, nil
			}},
	}
}

// carried is a skill of the job's agent, by name; any other is not there.
func carried(ctx context.Context, st skills, job db.Job, name string) (db.Skill, error) {
	if !job.AgentID.Valid {
		return db.Skill{}, brain.ErrNotFound
	}
	a, err := st.GetAgent(ctx, job.AgentID)
	if err != nil {
		return db.Skill{}, brain.ErrNotFound
	}
	var names []string
	_ = json.Unmarshal(a.Skills, &names)
	if !slices.Contains(names, name) {
		return db.Skill{}, brain.ErrNotFound
	}
	rows, err := st.SkillsNamed(ctx, db.SkillsNamedParams{Names: []string{name}, ProjectID: job.ProjectID})
	if err != nil {
		return db.Skill{}, err
	}
	if len(rows) == 0 {
		return db.Skill{}, brain.ErrNotFound
	}
	return rows[0], nil
}

type skillIn struct {
	Name string `json:"name" jsonschema:"the skill's name, as your prompt lists it"`
	Path string `json:"path,omitempty" jsonschema:"one of its files; empty for SKILL.md"`
}

type skillOut struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Path    string   `json:"path"`
	Content string   `json:"content"`
	Files   []string `json:"files"`
}
