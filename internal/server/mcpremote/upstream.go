// Package mcpremote is the remote MCP servers a project's agents reach — Sentry, Notion, Figma —
// and the proxy they reach them through. A runner never holds a remote server's credential: a
// job's MCP configuration points at <gator>/mcp/proxy/<server> with the job's own token, and the
// proxy checks the job's grant, holds it to the server's allowlist, puts the credential on the
// request it makes, writes every call down, stops calling a server that keeps failing, and hands
// back what the server said as data, fenced.
//
// Each of those is a piece of its own — Gate, Allowlist, Credentials, Recorder, Breakers, the
// fence — and the Proxy only puts them in order.
package mcpremote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/version"
)

var (
	// ErrForbidden is a caller with no grant to the server.
	ErrForbidden = errors.New("forbidden")
	// ErrNotFound is a server that is not there for the caller.
	ErrNotFound = errors.New("not found")
	// ErrDenied is a tool the allowlist does not hold.
	ErrDenied = errors.New("this tool is not enabled for you")
	// ErrNotConnected is a server whose credentials nobody has given yet.
	ErrNotConnected = errors.New("the server is not connected yet")
	// ErrInvalid is a request that does not hold together.
	ErrInvalid = errors.New("invalid")
	// ErrNoKeyring means credentials were given but GATOR_SECRETS_KEY is not configured.
	ErrNoKeyring = errors.New("remote MCP credentials need GATOR_SECRETS_KEY")
)

// Upstream is a remote MCP server as the proxy uses it. Every piece between the proxy and the
// server is an Upstream wrapping another.
type Upstream interface {
	ListTools(ctx context.Context) ([]*sdk.Tool, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (*sdk.CallToolResult, error)
}

// Dialer reaches a server. The Upstream it answers lives for one request; close ends it.
type Dialer interface {
	Dial(ctx context.Context, srv db.McpServer) (Upstream, func(), error)
}

// HTTPDialer reaches servers over Streamable HTTP, with their credentials on every request.
type HTTPDialer struct {
	Credentials Credentials
	// HTTP makes the requests; default: a 30 s timeout. Its transport carries the credentials.
	HTTP *http.Client
}

// Dial implements Dialer. Nothing is sent until the first call.
func (d HTTPDialer) Dial(_ context.Context, srv db.McpServer) (Upstream, func(), error) {
	base := http.DefaultTransport
	timeout := 30 * time.Second
	if d.HTTP != nil {
		if d.HTTP.Transport != nil {
			base = d.HTTP.Transport
		}
		if d.HTTP.Timeout > 0 {
			timeout = d.HTTP.Timeout
		}
	}
	client := &http.Client{Timeout: timeout, Transport: credentialTransport{base: base, srv: srv, creds: d.Credentials},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	up := &session{url: srv.Url, client: client}
	return up, up.close, nil
}

// session is one request's connection to a server, opened on first use.
type session struct {
	url    string
	client *http.Client
	mu     sync.Mutex
	cs     *sdk.ClientSession
}

func (s *session) open(ctx context.Context) (*sdk.ClientSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cs != nil {
		return s.cs, nil
	}
	c := sdk.NewClient(&sdk.Implementation{Name: "gator-proxy", Version: version.Version}, nil)
	cs, err := c.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: s.url, HTTPClient: s.client, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		return nil, err
	}
	s.cs = cs
	return cs, nil
}

func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cs != nil {
		_ = s.cs.Close()
		s.cs = nil
	}
}

// maxListPages bounds how far a server's paged tool list is read.
const maxListPages = 20

func (s *session) ListTools(ctx context.Context) ([]*sdk.Tool, error) {
	cs, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	var out []*sdk.Tool
	cursor := ""
	for range maxListPages {
		res, err := cs.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Tools...)
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	return out, nil
}

func (s *session) CallTool(ctx context.Context, name string, args json.RawMessage) (*sdk.CallToolResult, error) {
	cs, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	var a any = args
	if len(args) == 0 {
		a = map[string]any{}
	}
	return cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: a})
}
