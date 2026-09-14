// Package executor runs one leased job end to end on the runner: workspace, prompt, agent
// runs (resuming the session when a person steers), git results, cleanup and the receipt.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/runner/workspace"
)

// Executor implements client.Executor.
type Executor struct {
	WS       *workspace.Manager
	Backends map[string]backend.Backend
	Push     bool
	Log      *slog.Logger
}

var _ client.Executor = (*Executor)(nil)

// errSteered interrupts a run so it can resume with the person's correction.
var errSteered = errors.New("steered")

const maxSteers = 20

// AuthState implements client.AuthReporter.
func (e *Executor) AuthState() map[string]string {
	out := map[string]string{}
	for name, b := range e.Backends {
		out[name] = b.AuthState()
	}
	return out
}

// Run implements client.Executor.
func (e *Executor) Run(ctx context.Context, job proto.Job, io client.JobIO) proto.Finish {
	be, ok := e.Backends[job.Backend]
	if !ok {
		return proto.Finish{Status: proto.StatusFailed, StopReason: fmt.Sprintf("backend %q is not available on this runner", job.Backend)}
	}
	ws, err := e.WS.Prepare(ctx, job)
	if err != nil {
		return proto.Finish{Status: proto.StatusFailed, StopReason: "workspace: " + err.Error()}
	}
	io.Emit("workspace", map[string]any{"branch": ws.Branch, "base": ws.Base, "repo": repoName(job)})

	var usage proto.Usage
	var last backend.Outcome
	prompt, session, tools := Prompt(job, ws.HasRepo()), "", 0
	steers := 0
	for {
		runCtx, cancelRun := context.WithCancelCause(ctx)
		steer := make(chan string, 1)
		done := make(chan struct{})
		go func() {
			select {
			case m := <-io.Steer:
				steer <- m
				cancelRun(errSteered)
			case <-done:
			}
		}()
		budget := 0
		if job.Bounds.MaxToolCalls > 0 {
			budget = job.Bounds.MaxToolCalls - tools
			if budget < 1 {
				budget = 1
			}
		}
		last = be.Run(runCtx, backend.Spec{Dir: ws.Dir, Prompt: prompt, SessionID: session, MaxToolCalls: budget}, backend.Emit(io.Emit))
		close(done)
		cancelRun(nil)
		addUsage(&usage, last.Usage)
		tools += last.ToolCalls
		if last.SessionID != "" {
			session = last.SessionID
		}
		if last.Interrupted && errors.Is(last.Cause, errSteered) && ctx.Err() == nil && steers < maxSteers {
			msg := <-steer
			steers++
			io.Emit("steer_applied", map[string]any{"message": msg, "resume": session != ""})
			if session != "" {
				prompt = SteerPrompt(msg)
			} else {
				prompt = Prompt(job, ws.HasRepo()) + "\n\nA person added this correction before you started: " + msg
			}
			continue
		}
		break
	}

	fin := proto.Finish{Status: last.Status, StopReason: last.Reason, SessionID: session, Summary: last.Summary, Usage: usage}
	if last.Interrupted {
		cause := context.Cause(ctx)
		switch {
		case cause != nil && strings.HasPrefix(cause.Error(), "stopped"):
			fin.Status, fin.StopReason = proto.StatusStopped, cause.Error()
		case cause != nil:
			fin.Status, fin.StopReason = proto.StatusFailed, cause.Error()
		default:
			fin.Status = proto.StatusFailed
		}
	}

	// Git work happens even after a stop or timeout, on a context of its own: a person
	// should be able to see what the agent left behind.
	gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	commits, changed, dirty, gerr := ws.Changes(gctx)
	pushed, keep := false, false
	var pushErr string
	if gerr == nil && len(commits) > 0 {
		fin.Branch, fin.Commits, fin.ChangedFiles = ws.Branch, commits, changed
		if e.Push {
			if err := ws.Push(gctx); err != nil {
				pushErr, keep = err.Error(), true
			} else {
				pushed = true
			}
		} else {
			keep = true
		}
	}
	io.Emit("git", map[string]any{"commits": len(commits), "changed_files": changed, "uncommitted_files": dirty, "pushed": pushed, "push_error": pushErr})
	if err := ws.Cleanup(gctx, keep); err != nil && e.Log != nil {
		e.Log.Warn("workspace cleanup", "job", job.JobID, "err", err)
	}
	return fin
}

// Prompt is the opening instruction for a job. Role prompts from process/roles replace the
// generic framing in PLQ-224; the job's own instruction always comes through verbatim.
func Prompt(job proto.Job, hasRepo bool) string {
	var b strings.Builder
	title := job.TaskTitle
	if title == "" {
		title = "untitled task"
	}
	fmt.Fprintf(&b, "You are working as the %s on the task %q (phase: %s).\n\n", orDefault(job.Role, "agent"), title, orDefault(job.Phase, "unknown"))
	if strings.TrimSpace(job.Instruction) != "" {
		b.WriteString(strings.TrimSpace(job.Instruction))
		b.WriteString("\n\n")
	}
	if hasRepo {
		b.WriteString("Work in the current directory, a git checkout on its own branch. Commit your changes with clear messages; do not push, the runner does that. ")
	} else {
		b.WriteString("Work in the current directory. ")
	}
	b.WriteString("Finish with a short summary of what you did and what is left.")
	return b.String()
}

// SteerPrompt resumes a session with a person's correction.
func SteerPrompt(msg string) string {
	return "A person steering this job says:\n\n" + msg + "\n\nTake this into account and continue from where you stopped."
}

func addUsage(total *proto.Usage, u proto.Usage) {
	if total.Backend == "" {
		total.Backend = u.Backend
	}
	if u.Model != "" {
		total.Model = u.Model
	}
	total.InputTokens += u.InputTokens
	total.OutputTokens += u.OutputTokens
	total.CacheReadTokens += u.CacheReadTokens
	total.CacheWriteTokens += u.CacheWriteTokens
	total.CostUSD += u.CostUSD
	total.CostEstimated = total.CostEstimated || u.CostEstimated
	total.DurationMS += u.DurationMS
	if total.StartedAt.IsZero() || (!u.StartedAt.IsZero() && u.StartedAt.Before(total.StartedAt)) {
		total.StartedAt = u.StartedAt
	}
	if u.FinishedAt.After(total.FinishedAt) {
		total.FinishedAt = u.FinishedAt
	}
}

func repoName(job proto.Job) string {
	if job.Repo == nil {
		return ""
	}
	return job.Repo.Name
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
