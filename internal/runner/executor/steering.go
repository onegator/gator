package executor

import (
	"context"
	"errors"
	"sync"

	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/client"
)

// steering is how a run of a job hears what a person says while it works. A backend that keeps its
// session open (ActiveSteering) takes the message into the running turn; any other is interrupted
// and resumed with it. Either way, run answers what it could not deliver, for the executor to
// resume the session with.
type steering interface {
	run(ctx context.Context, spec backend.Spec, be backend.Backend, emit backend.Emit) (backend.Outcome, []string)
}

// steeringFor picks the steering a backend can do.
func steeringFor(be backend.Backend, io client.JobIO, cps *checkpoints, approve backend.Approver) steering {
	if be.Capabilities().ActiveSteering {
		return liveSteering{in: io.Steer, cps: cps, approve: approve}
	}
	return restartSteering{in: io.Steer, cps: cps}
}

// restartSteering interrupts a run when a person steers, and hands the message back to resume
// the session with it: the fallback for backends that run a prompt per process. Their turn ends
// with the run, so that is when the worktree is checkpointed.
type restartSteering struct {
	in  <-chan string
	cps *checkpoints
}

func (r restartSteering) run(ctx context.Context, spec backend.Spec, be backend.Backend, emit backend.Emit) (backend.Outcome, []string) {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	steered := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		select {
		case m := <-r.in:
			steered <- m
			cancel(errSteered)
		case <-done:
		}
	}()
	out := be.Run(runCtx, spec, emit)
	close(done)
	r.cps.afterTurn(ctx)
	if out.Interrupted && errors.Is(out.Cause, errSteered) && ctx.Err() == nil {
		return out, []string{<-steered}
	}
	return out, nil
}

// liveSteering hands the person's messages to the running session, which reads them into the turn
// without a restart; the worktree is checkpointed at the end of each of its turns. What the session
// could not deliver (it had closed) and what came after it ended go to a resumed run.
type liveSteering struct {
	in      <-chan string
	cps     *checkpoints
	approve backend.Approver
}

func (l liveSteering) run(ctx context.Context, spec backend.Spec, be backend.Backend, emit backend.Emit) (backend.Outcome, []string) {
	// A turn left open (the run was stopped, or died, before it answered) is checkpointed after.
	var mu sync.Mutex
	open := true
	track := func(typ string, payload any) {
		mu.Lock()
		switch typ {
		case "result":
			open = false
		case "tool_call", "text", "steer_applied":
			open = true
		}
		mu.Unlock()
		emit(typ, payload)
	}
	spec.Live = &backend.Live{Steer: l.in, Approve: l.approve, TurnEnded: func() { l.cps.afterTurn(ctx) }}
	out := be.Run(ctx, spec, track)
	mu.Lock()
	left := open
	mu.Unlock()
	if left {
		l.cps.afterTurn(ctx)
	}
	pending := append([]string(nil), out.Unread...)
drain:
	for {
		select {
		case m := <-l.in:
			pending = append(pending, m)
		default:
			break drain
		}
	}
	if ctx.Err() != nil {
		return out, nil
	}
	return out, pending
}
