package runners

import (
	"context"
	"encoding/json"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/store/db"
)

// maxSkillBytes bounds what of its agent's skills one lease carries, far below the runner
// connection's message limit: a skill that does not fit is left out and said so.
const maxSkillBytes = 2 << 20

// skillFile is one file of a skill as skills.files keeps it.
type skillFile struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

// skills are the skills the job's agent carries, whole, as the runner gives them to its backend.
// A summary has no agent and gets none.
func (s *session) skills(ctx context.Context, q *db.Queries, j db.Job, a db.Agent) []proto.Skill {
	if j.Kind == KindSummary {
		return nil
	}
	var names []string
	if json.Unmarshal(a.Skills, &names) != nil || len(names) == 0 {
		return nil
	}
	rows, err := q.SkillsNamed(ctx, db.SkillsNamedParams{Names: names, ProjectID: j.ProjectID})
	if err != nil {
		s.m.log.Warn("reading an agent's skills", "job", uuidString(j.ID), "err", err)
		return nil
	}
	var out []proto.Skill
	total := 0
	for _, row := range rows {
		var files []skillFile
		if err := json.Unmarshal(row.Files, &files); err != nil {
			continue
		}
		sk := proto.Skill{Name: row.Name, Description: row.Description}
		size := 0
		for _, f := range files {
			sk.Files = append(sk.Files, proto.SkillFile{Path: f.Path, Data: f.Data})
			size += len(f.Data)
		}
		if total+size > maxSkillBytes {
			s.m.log.Warn("an agent's skill does not fit in the lease", "job", uuidString(j.ID), "skill", row.Name, "bytes", size)
			continue
		}
		total += size
		out = append(out, sk)
	}
	return out
}
