package executor

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/client"
)

// approver holds a work job's tools to the project's approvals policy, for a backend that asks
// before a tool: what the policy denies is refused, what it asks about waits for a person (through
// the server, as a card in the conversation), and everything else a work agent may do as it always
// could. "Allow until the end of the session" lets the same rule through from then on. A job that
// only reads is never asked about anything: its tools do not widen because somebody could say yes.
type approver struct {
	ask     []backend.Rule
	deny    []backend.Rule
	timeout int
	request func(ctx context.Context, r proto.RuntimeRequest) proto.RuntimeDecision
	emit    func(typ string, payload any)

	mu      sync.Mutex
	session map[string]bool // ask rules allowed for the rest of the session
}

// approvals answers how a job's tools are held to its approvals policy: the approver a backend
// that asks is given (nil for none), and the rules a backend must refuse by itself. A backend that
// cannot ask (or a runner that cannot reach anyone) refuses what the policy asks about too — the
// agent is told no rather than let through unseen. A job that only reads has no approver: its
// session is never asked, and a prompt that comes anyway is refused. Nil approvals is what a job
// had before them: nothing.
func approvals(job proto.Job, be backend.Backend, io client.JobIO) (backend.Approver, []string) {
	a := job.Approvals
	if a == nil || job.ReadOnly() {
		return nil, nil
	}
	if !be.Capabilities().ToolApprovals || io.Request == nil {
		io.Emit("approvals_held_as_denials", map[string]any{"rules": len(a.Ask) + len(a.Deny)})
		return nil, append(append([]string{}, a.Ask...), a.Deny...)
	}
	return &approver{ask: backend.ParseRules(a.Ask), deny: backend.ParseRules(a.Deny), timeout: a.TimeoutSeconds,
		request: io.Request, emit: io.Emit, session: map[string]bool{}}, a.Deny
}

// Decide implements backend.Approver.
func (a *approver) Decide(ctx context.Context, r backend.ToolRequest) backend.Decision {
	if r.Kind == backend.RequestQuestion {
		return a.putToPerson(ctx, r, "")
	}
	if rule, ok := backend.Matching(a.deny, r.Tool, r.Input); ok {
		return backend.Decision{Message: fmt.Sprintf("The project does not allow this (%s). Do it another way, or say why it is needed.", rule.Raw)}
	}
	rule, ok := backend.Matching(a.ask, r.Tool, r.Input)
	if !ok {
		return backend.Decision{Allow: true}
	}
	a.mu.Lock()
	allowed := a.session[rule.Raw]
	a.mu.Unlock()
	if allowed {
		return backend.Decision{Allow: true, Message: "allowed for the session: " + rule.Raw}
	}
	return a.putToPerson(ctx, r, rule.Raw)
}

// putToPerson puts the request to a person and waits for the decision.
func (a *approver) putToPerson(ctx context.Context, r backend.ToolRequest, rule string) backend.Decision {
	req := proto.RuntimeRequest{ID: newID(), Kind: r.Kind, Tool: r.Tool, Input: boundedInput(r.Input), Rule: rule,
		ToolUseID: r.ToolUseID, TimeoutSeconds: a.timeout}
	a.emit("runtime_request", map[string]any{"id": req.ID, "kind": req.Kind, "tool": req.Tool, "rule": rule})
	d := a.request(ctx, req)
	a.emit("runtime_decision", map[string]any{"id": req.ID, "decision": d.Decision})
	switch d.Decision {
	case proto.DecisionAllowForSession:
		if rule != "" {
			a.mu.Lock()
			a.session[rule] = true
			a.mu.Unlock()
		}
		return backend.Decision{Allow: true}
	case proto.DecisionAllow:
		if r.Kind == backend.RequestQuestion {
			return backend.Decision{Answer: strings.TrimSpace(d.Feedback)}
		}
		return backend.Decision{Allow: true}
	case proto.DecisionStop:
		return backend.Decision{Stop: true, Message: decider(d) + " stopped the turn instead."}
	case proto.DecisionExpired:
		return backend.Decision{Message: "Nobody answered in time, so this is refused. Go on without it, or say why it is needed."}
	case proto.DecisionDeny:
		msg := decider(d) + " refused this."
		if f := strings.TrimSpace(d.Feedback); f != "" {
			msg += " Their reason: " + f
		}
		return backend.Decision{Message: msg}
	}
	return backend.Decision{Message: "Refused: the request could not be put to anyone (" + d.Feedback + ")."}
}

func decider(d proto.RuntimeDecision) string {
	if d.By != "" {
		return d.By
	}
	return "A person"
}

// boundedInput keeps a tool's input within what a request may carry; a longer one goes as a note
// of its length, and the person sees the tool and the rule.
func boundedInput(in []byte) []byte {
	if len(in) <= proto.MaxRequestInput {
		return in
	}
	return []byte(fmt.Sprintf(`{"note":"the input is %d bytes, too long to show"}`, len(in)))
}

// newID is a random UUID (v4), the request's own: a request sent again is the same one.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
