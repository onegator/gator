package executor

import (
	"context"
	"log/slog"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/runner/workspace"
)

// checkpointTimeout bounds one checkpoint. It runs after a stop too, on a context of its own, so
// a person sees what the turn left behind.
const checkpointTimeout = 2 * time.Minute

// checkpoints takes a checkpoint of a job's worktree before its first turn and after each one,
// and sends each turn's to the server. A job that reads (chat), has no repository or whose project
// turned them off has none; a checkpoint that fails is logged and the job goes on without it.
type checkpoints struct {
	ws      *workspace.Workspace
	send    func(proto.Checkpoint)
	emit    func(typ string, payload any)
	exclude []string
	repo    string
	log     *slog.Logger
	on      bool
	taken   []proto.CheckpointSummary
}

func newCheckpoints(job proto.Job, ws *workspace.Workspace, io client.JobIO, exclude []string, log *slog.Logger) *checkpoints {
	return &checkpoints{ws: ws, send: io.Checkpoint, emit: io.Emit, exclude: exclude, repo: repoName(job), log: log,
		on: io.Checkpoint != nil && !job.NoCheckpoints && !ws.ReadOnly && ws.HasRepo()}
}

// baseline records the worktree as the job found it.
func (c *checkpoints) baseline(ctx context.Context) { c.take(ctx) }

// afterTurn records what a turn left and sends it.
func (c *checkpoints) afterTurn(ctx context.Context) {
	cp := c.take(ctx)
	if cp == nil || cp.Seq == 0 {
		return
	}
	cp.Repo = c.repo
	c.send(cp.Checkpoint)
	sum := cp.Summary()
	c.taken = append(c.taken, sum)
	c.emit("checkpoint", map[string]any{"seq": sum.Seq, "files": sum.Files, "added": sum.Added, "removed": sum.Removed,
		"truncated": cp.Truncated, "checkpoint_ms": cp.TookMS})
}

func (c *checkpoints) take(ctx context.Context) *workspace.CheckpointResult {
	if !c.on {
		return nil
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkpointTimeout)
	defer cancel()
	cp, err := c.ws.Checkpoint(cctx, c.exclude)
	if err != nil {
		// Without the baseline no later checkpoint means anything: stop taking them.
		c.on = false
		if c.log != nil {
			c.log.Warn("checkpoint", "err", err)
		}
		c.emit("checkpoint_failed", map[string]any{"error": err.Error()})
		return nil
	}
	return cp
}

// summaries are the checkpoints sent, for the receipt.
func (c *checkpoints) summaries() []proto.CheckpointSummary { return c.taken }
