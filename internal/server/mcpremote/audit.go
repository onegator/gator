package mcpremote

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/limits"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store/db"
)

// Outcomes of a call.
const (
	OutcomeOK     = "ok"
	OutcomeError  = "error"  // the server failed, or the tool said it did
	OutcomeDenied = "denied" // the allowlist refused it
	OutcomeOpen   = "open"   // the breaker refused it
)

// Call is one call through the proxy, as it is written down.
type Call struct {
	Grant    Grant
	Method   string // tools/list, tools/call
	Tool     string
	Args     json.RawMessage
	Outcome  string
	Err      string
	Bytes    int
	Duration time.Duration
}

// Recorder writes calls down.
type Recorder interface {
	Record(ctx context.Context, c Call)
}

// callStore is where calls go. *db.Queries is one.
type callStore interface {
	InsertMCPCall(ctx context.Context, arg db.InsertMCPCallParams) error
}

// DBRecorder writes calls into mcp_calls, the arguments with anything that looks like a secret
// masked. A failure to write must not fail the call: the call already happened.
type DBRecorder struct {
	Store callStore
	Log   *slog.Logger
}

// Record implements Recorder.
func (r DBRecorder) Record(ctx context.Context, c Call) {
	var payload []byte
	if len(c.Args) > 0 {
		payload = secrets.RedactJSON(c.Args)
		if !json.Valid(payload) {
			payload = nil
		}
	}
	var why *string
	if c.Err != "" {
		why = &c.Err
	}
	g := c.Grant
	err := r.Store.InsertMCPCall(context.WithoutCancel(ctx), db.InsertMCPCallParams{McpServerID: g.Server.ID, ProjectID: g.Job.ProjectID,
		JobID: g.Job.ID, AgentID: g.Agent.ID, Method: c.Method, Tool: c.Tool, Payload: payload, Outcome: c.Outcome, Error: why,
		ResultBytes: int32(c.Bytes), DurationMs: int32(c.Duration.Milliseconds())})
	if err != nil && r.Log != nil {
		r.Log.Warn("mcp proxy: recording a call", "server", g.Server.Name, "tool", c.Tool, "err", err)
	}
}

// outcomeOf names how a call went.
func outcomeOf(err error, res *sdk.CallToolResult) string {
	switch {
	case errors.Is(err, ErrDenied):
		return OutcomeDenied
	case errors.Is(err, limits.ErrOpen):
		return OutcomeOpen
	case err != nil, res != nil && res.IsError:
		return OutcomeError
	}
	return OutcomeOK
}

// audited is an Upstream that writes every call down, whatever became of it.
type audited struct {
	Upstream
	rec   Recorder
	grant Grant
	now   func() time.Time
}

func (u audited) ListTools(ctx context.Context) ([]*sdk.Tool, error) {
	start := u.now()
	tools, err := u.Upstream.ListTools(ctx)
	b, _ := json.Marshal(tools)
	u.rec.Record(ctx, Call{Grant: u.grant, Method: "tools/list", Outcome: outcomeOf(err, nil), Err: errText(err), Bytes: len(b),
		Duration: u.now().Sub(start)})
	return tools, err
}

func (u audited) CallTool(ctx context.Context, name string, args json.RawMessage) (*sdk.CallToolResult, error) {
	start := u.now()
	res, err := u.Upstream.CallTool(ctx, name, args)
	b, _ := json.Marshal(res)
	why := errText(err)
	if why == "" && res != nil && res.IsError {
		why = "the tool answered with an error"
	}
	u.rec.Record(ctx, Call{Grant: u.grant, Method: "tools/call", Tool: name, Args: args, Outcome: outcomeOf(err, res), Err: why,
		Bytes: len(b), Duration: u.now().Sub(start)})
	return res, err
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
