package mcpremote

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store/db"
)

// Module is the remote MCP servers as the server wires them: the servers people keep, and the
// proxy agents reach them through.
type Module struct {
	Servers *Servers
	Proxy   *Proxy
	Keys    Keyring
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
}

// New builds the module.
func New(pool *pgxpool.Pool, d Deps) *Module {
	q := db.New(pool)
	keys := Keyring{Keys: d.Keys}
	dial := HTTPDialer{Credentials: keys, HTTP: d.HTTP}
	breakers, cache := NewBreakers(d.BreakerThreshold, d.BreakerCooldown), NewToolCache(0)
	proxy := NewProxy(ProxyDeps{Gate: JobGate{Store: q}, Dialer: dial, Recorder: DBRecorder{Store: q, Log: d.Log},
		Breakers: breakers, Cache: cache, MaxResult: d.MaxResult, Log: d.Log})
	return &Module{Servers: NewServers(q, auth.Authorizer{Pool: pool}, keys, dial, cache, breakers), Proxy: proxy, Keys: keys}
}
