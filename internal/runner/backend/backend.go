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
