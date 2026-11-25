// Package backend is the seam between the runner and agent CLIs. Each backend runs one
// agent process in a directory and reports what happened; the executor owns the job.
package backend

import (
	"context"

	"github.com/onegator/gator/internal/proto"
)

// Spec describes one agent run.
type Spec struct {
	Dir          string
	Prompt       string
	SessionID    string  // resume this session when set
	Model        string  // overrides the backend default
	MaxToolCalls int     // 0 = unlimited
	MaxCostUSD   float64 // dollars this run may spend, for backends that can cap it; 0 = no cap
	// Env is added to the agent's environment, as KEY=VALUE. It carries the job's own identity
	// for gator-cli, and the directory that holds it.
	Env []string
	// DenyRead are files the agent must not open: the runner's token file. Backends pass them
	// to agentenv.Command, which enforces it where the platform can.
	DenyRead []string
	// Access is what the agent may do with its tools. The zero value is everything, as a phase
	// job always could.
	Access Access
}

// Access is what an agent may do with its tools, in words every backend can translate into its
// own flags. It is what the tools allow, not a sandbox: the workspace is made read-only too.
type Access struct {
	// ReadOnly gives the agent tools that read, search and fetch, and none that write.
	ReadOnly bool
	// NoTools gives it none at all: a model reading what it is given and answering.
	NoTools bool
	// Commands are the only commands a read-only agent may run, by name: gator-cli, to talk back.
	Commands []string
}

// Emit streams one event.
type Emit func(typ string, payload any)

// Outcome is what one run produced.
type Outcome struct {
	Status      string // proto.StatusDone | proto.StatusFailed; empty when Interrupted
	Reason      string
	Interrupted bool  // the caller's context ended the run (stop, timeout, steer)
	Cause       error // context cause when Interrupted
	SessionID   string
	Model       string
	ToolCalls   int
	Summary     string
	Usage       proto.Usage
}

// Backend runs an agent CLI.
type Backend interface {
	Name() string
	Run(ctx context.Context, spec Spec, emit Emit) Outcome
	// AuthState reports the login of this backend: "ok", "missing" (the command is there, the
	// credentials are not), "no_cli" (the command itself is not on this runner's PATH) or
	// "unknown". A missing command and a missing login are different problems with different
	// fixes, and reporting the first as the second sends a person to log in again on a machine
	// they were already logged in on.
	AuthState() string
}

// Limited is a backend that can hold an agent to less than full access. A backend says which
// limits it can keep; one that cannot keep a limit never runs a job that needs it.
type Limited interface {
	Supports(a Access) bool
}

// Supports says whether b can run an agent with access a: full access always, anything less only
// where b says it can keep it.
func Supports(b Backend, a Access) bool {
	if !a.ReadOnly && !a.NoTools {
		return true
	}
	l, ok := b.(Limited)
	return ok && l.Supports(a)
}
