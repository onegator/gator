package marketplace

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/netguard"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// DefaultURL is Gator's own registry, and DefaultPublicKey the key its index is signed with. The
// private key never leaves its maintainers.
const (
	DefaultURL       = "https://github.com/onegator/gator-marketplace"
	DefaultPublicKey = "ed25519:n5hZvq3lwgn0Q1GakLg8p9MWKrhmS8VrBFu7wNOwNqk="
)

// Module is the marketplace as the server wires it.
type Module struct {
	Catalog       *Catalog
	Installations *Installations
}

// Deps are what the module is built from; zero values take the defaults.
type Deps struct {
	Fetcher Fetcher // default: FetcherFor(Net)
	Limits  Limits  // default: DefaultLimits
	Kinds   Kinds   // default: every kind this server knows
	// Roles answers people's roles in projects; default auth.Authorizer over the pool.
	Roles roles
	// Capabilities says what a project's plugins can do, for what a package needs; nil: nothing
	// is reported missing for want of a capability.
	Capabilities capabilities
	// Plugins installs native plugins (*plugins.Host), whose builds go to PluginDir; either
	// missing: native plugins are not installed from the marketplace.
	Plugins   pluginHost
	PluginDir string
	// Download brings native builds; default HTTPDownloader. Platform is GOOS/GOARCH; default
	// this server's.
	Download Downloader
	Platform string
	// Net is what addresses registries and downloads may have (GATOR_OUTBOUND_ALLOW_PRIVATE);
	// AllowFileDownloads lets packages name builds on the server's disk
	// (GATOR_MARKETPLACE_ALLOW_FILE).
	Net                netguard.Policy
	AllowFileDownloads bool
}

// DefaultKinds are the kinds a registry may offer.
func DefaultKinds() Kinds {
	return KindsOf(Agent{}, Skill{}, Integration{}, Process{}, Knowledge{})
}

// New builds the module.
func New(pool *pgxpool.Pool, deps Deps) *Module {
	if deps.Fetcher == nil {
		deps.Fetcher = FetcherFor(deps.Net)
	}
	if deps.Limits == (Limits{}) {
		deps.Limits = DefaultLimits
	}
	if deps.Kinds == nil {
		deps.Kinds = DefaultKinds()
	}
	if deps.Roles == nil {
		deps.Roles = auth.Authorizer{Pool: pool}
	}
	q := db.New(pool)
	catalog := NewCatalog(q, deps.Fetcher, deps.Kinds, deps.Limits)
	skills := SkillInstaller{}
	if deps.Download == nil {
		deps.Download = HTTPDownloader{Net: deps.Net, AllowFile: deps.AllowFileDownloads}
	}
	native := NativeTrack{Dir: deps.PluginDir, Platform: deps.Platform, Download: deps.Download, Host: deps.Plugins, Plugins: db.New(pool)}
	tx := func(ctx context.Context, fn func(q *db.Queries) error) error { return store.InTx(ctx, pool, fn) }
	return &Module{
		Catalog: catalog,
		Installations: NewInstallations(q, catalog, deps.Roles, deps.Capabilities, tx,
			NewAgentInstaller(skills), skills, NewIntegrationInstaller(MCPTrack{}, native), ProcessInstaller{}, KnowledgeInstaller{}),
	}
}

// Start puts Gator's own registry in place and fetches it, so a fresh server has a catalogue.
// A fetch that fails is recorded on the registry and tried again when a person asks.
func (m *Module) Start(ctx context.Context, b Builtin) error {
	r, err := m.Catalog.EnsureBuiltin(ctx, b)
	if err != nil {
		return err
	}
	_, err = m.Catalog.refresh(ctx, r.ID)
	return err
}

// BuiltinFromEnv reads Gator's own registry from the server's environment:
// GATOR_MARKETPLACE_URL (default DefaultURL) and GATOR_MARKETPLACE_PUBLIC_KEY (default
// DefaultPublicKey). A server pointed at a mirror says the mirror's key, or the same key.
func BuiltinFromEnv(getenv func(string) string) Builtin {
	b := Builtin{Name: "gator", URL: getenv("GATOR_MARKETPLACE_URL"), PublicKey: getenv("GATOR_MARKETPLACE_PUBLIC_KEY")}
	if b.URL == "" {
		b.URL = DefaultURL
	}
	if b.PublicKey == "" {
		b.PublicKey = DefaultPublicKey
	}
	return b
}
