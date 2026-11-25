package mcp

import (
	"context"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/jobscope"
)

// Who may call a tool.
type access struct {
	// command is what a job's identity needs to call it (jobscope); empty: no job may.
	command jobscope.Command
	// people says a person's token may call it.
	people bool
}

func (a access) allowed(c Caller) bool {
	if c.IsJob() {
		return a.command != "" && c.scope[a.command]
	}
	return a.people
}

// tool is a Tool from a typed function: its input is read and checked against the schema the SDK
// infers from In, and its output is the call's structured result.
type tool[In, Out any] struct {
	name  string
	about string
	who   access
	call  func(ctx context.Context, c Caller, in In) (Out, map[string]any, error)
}

func (t tool[In, Out]) Name() string          { return t.name }
func (t tool[In, Out]) Allowed(c Caller) bool { return t.who.allowed(c) }

func (t tool[In, Out]) Add(s *sdk.Server, c Caller, rec Recorder) {
	sdk.AddTool(s, &sdk.Tool{Name: t.name, Description: t.about}, func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
		out, fields, err := t.call(ctx, c, in)
		if fields == nil {
			fields = map[string]any{}
		}
		if err != nil {
			fields["error"] = err.Error()
		}
		rec(ctx, c, t.name, fields)
		return nil, out, plain(err)
	})
}

// plain is an error as the agent reads it: what was refused, in a sentence, and nothing of the
// server's insides.
func plain(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, brain.ErrNotFound):
		return errors.New("not found")
	case errors.Is(err, brain.ErrForbidden):
		return errors.New("not allowed here")
	case errors.Is(err, brain.ErrConflict):
		return errors.New("somebody changed this entry at the same moment; try again")
	case brain.IsInvalid(err), errors.As(err, new(refusal)):
		return err
	}
	return errors.New("Gator could not do that just now; try again or carry on without it")
}

// refusal is a sentence a tool answers with when it will not do something.
type refusal string

func (r refusal) Error() string { return string(r) }
