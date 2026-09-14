package runners

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/store/db"
)

// WorkingStateType is the artifact that carries a task's progress from job to job, whatever
// backend or model runs next. It lives in the pseudo-phase "task".
const WorkingStateType = "working_state"

// refreshWorkingState rebuilds the working state from the receipts of every done job of the
// task and stores it as a new artifact version. It is recomputed, never patched, so it cannot
// drift from the receipts.
func refreshWorkingState(ctx context.Context, q *db.Queries, taskID pgtype.UUID) error {
	jobs, err := q.ListJobsByTask(ctx, taskID)
	if err != nil {
		return err
	}
	type entry struct {
		job db.Job
		fin proto.Finish
	}
	var done []entry
	for _, j := range jobs {
		if j.Status != proto.StatusDone || len(j.Receipt) == 0 {
			continue
		}
		var f proto.Finish
		if json.Unmarshal(j.Receipt, &f) == nil {
			done = append(done, entry{j, f})
		}
	}
	if len(done) == 0 {
		return nil
	}
	var b strings.Builder
	last := done[len(done)-1]
	b.WriteString("# Working state\n\n")
	fmt.Fprintf(&b, "_After the %s job in %s on %s, %s._\n", last.job.Role, last.job.Phase, last.job.Backend, last.job.FinishedAt.Time.UTC().Format("2006-01-02 15:04 UTC"))
	for i := len(done) - 1; i >= 0; i-- {
		if f := done[i].fin; f.Branch != "" && len(f.Commits) > 0 {
			fmt.Fprintf(&b, "\nLatest code: branch `%s`, commit `%s`.\n", f.Branch, short(f.Commits[len(f.Commits)-1]))
			break
		}
	}
	section := func(title string, pick func(proto.Digest) []string, onlyLast bool) {
		b.WriteString("\n## " + title + "\n\n")
		seen := map[string]bool{}
		n := 0
		from := 0
		if onlyLast {
			from = len(done) - 1
		}
		for _, e := range done[from:] {
			if e.fin.Digest == nil {
				continue
			}
			for _, item := range pick(*e.fin.Digest) {
				if seen[item] {
					continue
				}
				seen[item] = true
				n++
				if onlyLast {
					fmt.Fprintf(&b, "- %s\n", item)
				} else {
					fmt.Fprintf(&b, "- *%s/%s:* %s\n", e.job.Phase, e.job.Role, item)
				}
			}
		}
		if n == 0 {
			if onlyLast && last.fin.Digest == nil {
				b.WriteString("- _The last job left no digest._\n")
			} else {
				b.WriteString("- _Nothing recorded._\n")
			}
		}
	}
	section("Done so far", func(d proto.Digest) []string { return d.Changes }, false)
	section("Decisions", func(d proto.Digest) []string { return d.Decisions }, false)
	section("Rejected approaches", func(d proto.Digest) []string { return d.Rejected }, false)
	section("What is left", func(d proto.Digest) []string { return d.Left }, true)

	content := b.String()
	_, err = q.CreateArtifact(ctx, db.CreateArtifactParams{TaskID: taskID, Phase: "task", Type: WorkingStateType, Content: &content})
	return err
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
