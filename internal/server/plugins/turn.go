package plugins

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// maxObjectionBytes bounds one objection. It is read by a model as data, and a plugin that
// pastes a whole test log into it would push the work out of the agent's own context.
const maxObjectionBytes = 4000

// Objections asks the project's plugins whether this turn may end. It implements
// runners.TurnJudge.
//
// The order matters more than it looks: a plugin that knows the tests never ran can say so
// while the agent is still in its session, with the worktree in front of it, instead of the
// gate refusing the phase ten minutes later and a person reading a report about work that was
// never finished. A plugin that fails or times out is silent, as everywhere else — a broken
// plugin must not be able to hold an agent in a loop.
func (h *Host) Objections(ctx context.Context, job db.Job, te proto.TurnEnding) []proto.Objection {
	insts := h.forProject(job.ProjectID, plugin.MethodTurnEnding)
	if len(insts) == 0 {
		return nil
	}
	t, err := db.New(h.pool).GetTask(ctx, job.TaskID)
	if err != nil {
		return nil
	}
	var digest json.RawMessage
	if te.Digest != nil {
		digest, _ = json.Marshal(te.Digest)
	}
	params := plugin.TurnEndingParams{
		Task: taskRef(t), Job: jobRef(job), Turn: te.Turn, Status: te.Status, Summary: te.Summary,
		Commits: te.Commits, ChangedPaths: te.ChangedPaths, Digest: digest,
	}
	var (
		mu  sync.Mutex
		out []proto.Objection
	)
	h.each(insts, func(i *instance) {
		var res plugin.TurnEndingResult
		if err := h.call(ctx, i, plugin.MethodTurnEnding, params, &res); err != nil {
			if ctx.Err() == nil {
				i.log().Warn("asking a plugin whether a turn may end", "job", uuidString(job.ID), "err", err)
			}
			return
		}
		reason := res.Objection
		if reason == "" {
			return
		}
		if len(reason) > maxObjectionBytes {
			reason = reason[:maxObjectionBytes] + "\n…(the rest was cut)"
		}
		mu.Lock()
		defer mu.Unlock()
		out = append(out, proto.Objection{Source: i.b.manifest.Name, Reason: reason})
	})
	// Plugins answer in whatever order they finish; the agent should read them in the same
	// order every time, so a rerun of the same turn reads the same way.
	sort.Slice(out, func(a, b int) bool { return out[a].Source < out[b].Source })
	return out
}
