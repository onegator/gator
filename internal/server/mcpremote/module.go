package mcpremote

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/netguard"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// Module is the remote MCP servers as the server wires them: the servers people keep, and the
// proxy agents reach them through.
type Module struct {
	Servers *Servers
	Proxy   *Proxy
	// OAuth connects servers that sign in with OAuth (through /connect/callback, as a
	// plugins.Connector) and renews their tokens.
	OAuth *OAuth
	Keys  Keyring
}

// Deps are what the module is built from; zero values take the defaults.
type Deps struct {
	Keys *secrets.Keyring
	// HTTP makes the proxy's requests to remote servers; default a 30 s timeout.
	HTTP *http.Client
	// BreakerThreshold failures in a row stop calls to a server for BreakerCooldown; defaults 5
	// and a minute.
	BreakerThreshold int
	BreakerCooldown  time.Duration
	MaxResult        int // bytes of one answer an agent gets; default DefaultMaxResult
	Log              *slog.Logger
	// PublicURL is where providers send the browser back (PublicURL/connect/callback); empty:
	// no server can be connected with OAuth.
	PublicURL string
	// Apps are the server's OAuth apps by provider; default: none.
	Apps func(provider string) (OAuthApp, bool)
	// OAuthHTTP makes the requests to providers; default a 20 s timeout over Net's transport.
	OAuthHTTP *http.Client
	// Net is what addresses remote servers and providers may have (GATOR_OUTBOUND_ALLOW_PRIVATE).
	Net netguard.Policy
}

// New builds the module.
func New(pool *pgxpool.Pool, d Deps) *Module {
	q := db.New(pool)
	keys := Keyring{Keys: d.Keys}
	breakers, cache := NewBreakers(d.BreakerThreshold, d.BreakerCooldown), NewToolCache(0)
	roles := auth.Authorizer{Pool: pool}
	if d.OAuthHTTP == nil {
		d.OAuthHTTP = d.Net.Client(20 * time.Second)
	}
	oauth := NewOAuth(q, roles, keys, d.Apps, d.PublicURL, d.OAuthHTTP, cache)
	dial := HTTPDialer{Credentials: oauth, HTTP: d.HTTP, Net: d.Net}
	proxy := NewProxy(ProxyDeps{Gate: JobGate{Store: q}, Dialer: dial, Recorder: DBRecorder{Store: q, Log: d.Log},
		Breakers: breakers, Cache: cache, MaxResult: d.MaxResult, Log: d.Log})
	return &Module{Servers: NewServers(q, roles, keys, dial, cache, breakers, d.Net,
		func(ctx context.Context, fn func(*db.Queries) error) error { return store.InTx(ctx, pool, fn) }), Proxy: proxy, OAuth: oauth, Keys: keys}
}
