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
		last = be.Run(runCtx, backend.Spec{Dir: ws.Dir, Prompt: prompt, SessionID: session, Model: job.Model, MaxToolCalls: budget, MaxCostUSD: costLeft(job, usage)}, backend.Emit(io.Emit))
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

	// The cost cap holds for every backend, including those that cannot enforce it mid-run.
	if limit := job.Bounds.MaxCostUSD; limit > 0 && usage.CostUSD > limit && last.Status == proto.StatusDone {
		last.Status, last.Reason = proto.StatusFailed, fmt.Sprintf("cost bound ($%.2f) exceeded: $%.2f", limit, usage.CostUSD)
	}

	digest, summary := ExtractDigest(last.Summary)
	fin := proto.Finish{Status: last.Status, StopReason: last.Reason, SessionID: session, Summary: summary, Usage: usage}
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

	// A done session that forgot its digest is asked once more, in the same session: cheap,
	// because the context is cached, and it keeps the working state complete.
	if fin.Status == proto.StatusDone && digest == nil && session != "" && ctx.Err() == nil {
		io.Emit("digest_requested", map[string]any{"session_id": session})
		out := be.Run(ctx, backend.Spec{Dir: ws.Dir, Prompt: DigestPrompt, SessionID: session, Model: job.Model, MaxToolCalls: 1}, backend.Emit(io.Emit))
		addUsage(&usage, out.Usage)
		fin.Usage = usage
		if out.Status == proto.StatusDone {
			digest, _ = ExtractDigest(out.Summary)
		}
	}
	fin.Digest = digest

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

// Prompt is the opening instruction for a job: the role's guide (resolved by the server per
// project), the task and what the person asked for, the job's own instruction, and earlier
// documents. Without a guide it falls back to a generic framing.
func Prompt(job proto.Job, hasRepo bool) string {
	var b strings.Builder
	title := job.TaskTitle
	if title == "" {
		title = "untitled task"
	}
	guide := strings.TrimSpace(job.Guide)
	if guide != "" {
		b.WriteString(guide)
		b.WriteString("\n\n---\n\n")
		fmt.Fprintf(&b, "Task: %q (phase: %s, your role: %s)\n", title, orDefault(job.Phase, "unknown"), orDefault(job.Role, "agent"))
	} else {
		fmt.Fprintf(&b, "You are working as the %s on the task %q (phase: %s).\n", orDefault(job.Role, "agent"), title, orDefault(job.Phase, "unknown"))
	}
	if d := strings.TrimSpace(job.TaskDescription); d != "" {
		b.WriteString("\nWhat the person asked for:\n\n")
		b.WriteString(d)
		b.WriteString("\n")
	}
	if i := strings.TrimSpace(job.Instruction); i != "" {
		b.WriteString("\nInstruction for this job:\n\n")
		b.WriteString(i)
		b.WriteString("\n")
	}
	for _, doc := range job.Context {
		fmt.Fprintf(&b, "\n## Context: %s\n\n%s\n", doc.Title, strings.TrimSpace(doc.Body))
	}
	b.WriteString("\n")
	switch {
	case hasRepo && guide != "":
		// The role guide decides whether to change anything; this line only states the facts,
		// so a read-only role is never told to commit.
		b.WriteString("The current directory is a git checkout of the project on its own branch; the runner pushes that branch when you finish.")
	case hasRepo:
		b.WriteString("Work in the current directory, a git checkout on its own branch. Commit your changes with clear messages; do not push, the runner does that.")
	default:
		b.WriteString("Work in the current directory. There is no repository for this job.")
	}
	if guide == "" {
		b.WriteString(" Finish with a short summary of what you did and what is left.")
	}
	b.WriteString("\n\n")
	b.WriteString(DigestInstruction)
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

// costLeft is what the job may still spend in its next run; 0 = no cap.
func costLeft(job proto.Job, used proto.Usage) float64 {
	if job.Bounds.MaxCostUSD <= 0 {
		return 0
	}
	if left := job.Bounds.MaxCostUSD - used.CostUSD; left > 0.01 {
		return left
	}
	return 0.01
}
