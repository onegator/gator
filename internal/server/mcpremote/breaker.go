package mcpremote

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/limits"
)

// Breakers hold one limits.Breaker per server, as plugins have: a server that keeps failing is
// not called for a while, so a job does not spend its turn waiting on it, and the server is not
// hammered while it is down.
type Breakers struct {
	Threshold int           // failures in a row; default 5
	Cooldown  time.Duration // default a minute

	mu sync.Mutex
	m  map[[16]byte]*limits.Breaker
}

// NewBreakers builds them.
func NewBreakers(threshold int, cooldown time.Duration) *Breakers {
	if threshold == 0 {
		threshold = 5
	}
	if cooldown == 0 {
		cooldown = time.Minute
	}
	return &Breakers{Threshold: threshold, Cooldown: cooldown, m: map[[16]byte]*limits.Breaker{}}
}

// For is a server's breaker.
func (b *Breakers) For(server pgtype.UUID) *limits.Breaker {
	b.mu.Lock()
	defer b.mu.Unlock()
	br, ok := b.m[server.Bytes]
	if !ok {
		br = limits.NewBreaker(b.Threshold, b.Cooldown)
		b.m[server.Bytes] = br
	}
	return br
}

// State is a server's breaker's state, for its admin.
func (b *Breakers) State(server pgtype.UUID) limits.State { return b.For(server).State() }

// Reset closes a server's breaker: its admin fixed it.
func (b *Breakers) Reset(server pgtype.UUID) { b.For(server).Reset() }

// guarded is an Upstream behind a breaker. What fails is the server not answering, or answering
// what is not MCP. A tool that says it failed (isError), or a JSON-RPC error the server sends
// back (arguments it refuses), is the server's answer, not its failure.
type guarded struct {
	Upstream
	breaker *limits.Breaker
}

func (u guarded) ListTools(ctx context.Context) ([]*sdk.Tool, error) {
	var tools []*sdk.Tool
	var callErr error
	err := u.breaker.Do(func() error {
		tools, callErr = u.Upstream.ListTools(ctx)
		return failed(callErr)
	})
	if err != nil && callErr == nil {
		return nil, err // the breaker's own refusal
	}
	return tools, callErr
}

func (u guarded) CallTool(ctx context.Context, name string, args json.RawMessage) (*sdk.CallToolResult, error) {
	var res *sdk.CallToolResult
	var callErr error
	err := u.breaker.Do(func() error {
		res, callErr = u.Upstream.CallTool(ctx, name, args)
		return failed(callErr)
	})
	if err != nil && callErr == nil {
		return nil, err
	}
	return res, callErr
}

// rejectedByTransport is the code the SDK wraps a request the transport could not deliver in (a
// connection refused, a 5xx): that is the server failing, not answering.
const rejectedByTransport = -32005

// failed is what of err counts against the server: anything but an answer it gave.
func failed(err error) error {
	if we, ok := errors.AsType[*jsonrpc.Error](err); ok && we.Code != rejectedByTransport {
		return nil
	}
	return err
}
