package mcpremote

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolCache keeps each server's tool list for a while: every request through the proxy needs it,
// and asking the server each time would double the calls it gets.
type ToolCache struct {
	TTL time.Duration // default a minute
	now func() time.Time

	mu    sync.Mutex
	lists map[[16]byte]cachedList
}

type cachedList struct {
	tools []*sdk.Tool
	at    time.Time
}

// NewToolCache builds one.
func NewToolCache(ttl time.Duration) *ToolCache {
	if ttl == 0 {
		ttl = time.Minute
	}
	return &ToolCache{TTL: ttl, now: time.Now, lists: map[[16]byte]cachedList{}}
}

// Forget drops a server's list: its admin changed it.
func (c *ToolCache) Forget(server pgtype.UUID) {
	c.mu.Lock()
	delete(c.lists, server.Bytes)
	c.mu.Unlock()
}

func (c *ToolCache) get(server pgtype.UUID) ([]*sdk.Tool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.lists[server.Bytes]
	if !ok || c.now().Sub(l.at) > c.TTL {
		return nil, false
	}
	return l.tools, true
}

func (c *ToolCache) put(server pgtype.UUID, tools []*sdk.Tool) {
	c.mu.Lock()
	c.lists[server.Bytes] = cachedList{tools: tools, at: c.now()}
	c.mu.Unlock()
}

// cached is an Upstream whose tool list comes from the cache while it is fresh.
type cached struct {
	Upstream
	cache  *ToolCache
	server pgtype.UUID
}

func (u cached) ListTools(ctx context.Context) ([]*sdk.Tool, error) {
	if tools, ok := u.cache.get(u.server); ok {
		return tools, nil
	}
	tools, err := u.Upstream.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	u.cache.put(u.server, tools)
	return tools, nil
}
