// gator-server is the brain: it owns process state, talks to runners and plugins,
// and serves the API used by Gator.app.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/onegator/gator/internal/server/a2a"
	"github.com/onegator/gator/internal/server/admin"
	"github.com/onegator/gator/internal/server/agents"
	"github.com/onegator/gator/internal/server/api"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/automations"
	"github.com/onegator/gator/internal/server/backup"
	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/brain/curator"
	"github.com/onegator/gator/internal/server/bridges"
	"github.com/onegator/gator/internal/server/config"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/jobs"
	"github.com/onegator/gator/internal/server/knowledge"
	"github.com/onegator/gator/internal/server/marketplace"
	"github.com/onegator/gator/internal/server/mcp"
	"github.com/onegator/gator/internal/server/mcpremote"
	"github.com/onegator/gator/internal/server/netguard"
	"github.com/onegator/gator/internal/server/notify"
	"github.com/onegator/gator/internal/server/plugins"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/product"
	"github.com/onegator/gator/internal/server/provision"
	"github.com/onegator/gator/internal/server/quality"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store"
	dbq "github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/server/telemetry"
	"github.com/onegator/gator/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gator-server:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gator-server <serve|migrate|migrate-down|admin create-admin <email> [name]|admin issue-runner-token <name>|backup now|restore <file>|secrets new-key [id]|secrets check|marketplace …|version>")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "version":
		fmt.Printf("gator-server %s (%s, %s)\n", version.Version, version.Commit, version.Date)
		return nil
	case "migrate":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return store.Migrate(ctx, cfg.DatabaseURL)
	case "migrate-down":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return store.MigrateDown(ctx, cfg.DatabaseURL)
	case "serve":
		return serve(ctx)
	case "knowledge":
		return knowledgeCmd(args[1:])
	case "marketplace":
		return marketplaceCmd(args[1:])
	case "secrets":
		return secretsCmd(args[1:])
	case "admin":
		return adminCmd(ctx, args[1:])
	case "backup":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		bc := backup.LoadConfig()
		if !bc.Enabled() {
			return errors.New("backups not configured (GATOR_BACKUP_S3_*)")
		}
		return backup.Run(ctx, bc, cfg.DatabaseURL, slog.Default())
	case "restore":
		if len(args) < 2 {
			return errors.New("usage: gator-server restore <dump file>")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return backup.Restore(ctx, cfg.DatabaseURL, args[1])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(secrets.RedactingHandler{Inner: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)})})
	slog.SetDefault(log)

	keyring, err := secrets.LoadKeyring()
	if err != nil {
		return err
	}
	shutdownTelemetry, err := telemetry.Setup(ctx, "gator-server", version.Version)
	if err != nil {
		return err
	}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTelemetry(tctx)
	}()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	catalog, err := process.DefaultCatalog()
	if err != nil {
		return err
	}
	// A project's capabilities are those of its enabled plugins.
	svc := process.NewService(db.Pool, process.DBTemplates{Pool: db.Pool, Defaults: catalog}, plugins.Capabilities{Pool: db.Pool})
	hub := events.NewHub()
	// Conversation events reach the people in the conversation (the "me" topic) as they go out.
	relay := &events.Relay{Pool: db.Pool, Hub: hub, Log: log, Audience: conversations.NewRelayAudience(dbq.New(db.Pool))}
	go relay.Run(ctx)
	// The catalogue of things Gator does for a project by itself once a person switches them on.
	autos := automations.New(db.Pool, svc, hub, log)
	go autos.Run(ctx)
	// People talk in a project's channels; the consumer keeps them in step with the project.
	// Files people send are kept in a bucket or a directory, or not at all.
	blobs, err := conversations.BlobStoreFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	if blobs == nil {
		log.Warn("attachments not configured (GATOR_ATTACHMENTS_DIR or GATOR_ATTACHMENTS_S3_*)")
	}
	chat := conversations.New(db.Pool, conversations.Deps{Roles: auth.Authorizer{Pool: db.Pool}, Decisions: svc, Tasks: svc, Blobs: blobs,
		// What the Brain was proposed from a task, one card a day in its thread.
		CardSources: []conversations.CardSource{curator.NewCards(dbq.New(db.Pool))}})
	// What the team and its agents know: one store, read under the same rules everywhere.
	knows := brain.New(db.Pool, brain.Deps{Conversations: chat.Access})
	// Jobs are handed their part of it, and the knowledge shelves are read from it.
	svc.UseBrain(knows.Packer, knows.Legacy)
	// Plugins propose to the Brain too (brain.propose), as proposals a person approves.
	// Remote MCP servers agents reach through Gator's proxy; their credentials stay here, and one
	// that signs in with OAuth is connected through the plugins' /connect/callback.
	// The server's own requests to remote servers, providers, registries and downloads stay off
	// private addresses unless the operator says otherwise.
	outbound := netguard.FromEnv(os.Getenv)
	remote := mcpremote.New(db.Pool, mcpremote.Deps{Keys: keyring, Log: log, PublicURL: cfg.PublicURL, Apps: oauthApps, Net: outbound})
	// Messaging plugins (Slack, GitHub comments) bring outside conversations in and mirror ours out,
	// through the core's rules: their own project, what they connected, untrusted words, no echo.
	bridge := bridges.New(db.Pool, bridges.Deps{Threads: chat.Threads, Writer: chat.Bridged, Rights: chat.Access,
		Roles: auth.Authorizer{Pool: db.Pool}, Approver: svc})
	pluginCfg := plugins.Config{PublicURL: cfg.PublicURL, Automations: autos, Brain: knows.Plugins,
		Connectors: map[string]plugins.Connector{plugins.TargetMCP: remote.OAuth},
		Extensions: []plugins.Extension{{Capability: bridge.Capability(), Methods: bridge.Methods()}},
		EventHooks: []plugins.EventHook{bridge}, AllowPrivateOutbound: outbound.AllowPrivate}
	pluginCfg.LoadEnv()
	pluginHost := plugins.New(db.Pool, svc, keyring, hub, log, pluginCfg)
	go pluginHost.Run(ctx)
	// Push notifications: without an APNs key the sender is disabled and nothing leaves here.
	var sender notify.Sender = notify.Disabled{}
	if apns, err := notify.LoadAPNs(); err != nil {
		return err
	} else if apns != nil {
		sender = apns
	}
	notifier := notify.New(db.Pool, svc, sender, hub, log, 0)
	// Finished work proposes what the product learned; a person approves it.
	learner := product.NewCuratorFor(db.Pool, knows.Service, hub, log, 0)
	// Once a project has learned more than a prompt can carry, a curator proposes a shorter
	// version of it; the originals are archived, never deleted.
	condenser := product.NewCondenser(learner, svc, 0, 0)
	go learner.Run(ctx)

	bc := backup.LoadConfig()
	scorecards := quality.New(db.Pool, svc, pluginHost, log)
	queue, err := jobs.New(jobs.Options{Pool: db.Pool, Process: svc, Backup: bc, DatabaseURL: cfg.DatabaseURL,
		Plugins: pluginHost, Releases: pluginHost, Quality: scorecards, Product: condenser, Automations: autos, Log: log})
	if err != nil {
		return err
	}
	// Start the queue on a context SIGTERM does not cancel: River treats a cancelled start
	// context as a hard stop. Shutdown below stops it softly, then forces it after the timeout.
	if err := queue.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("start queue: %w", err)
	}
	if bc.Enabled() {
		log.Info("backups enabled", "bucket", bc.Bucket, "endpoint", bc.Endpoint)
	} else {
		log.Warn("backups not configured (GATOR_BACKUP_S3_*)")
	}

	cloud, err := provision.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	if cloud.Provisioner != nil {
		log.Info("cloud runners start on demand", "provisioner", cloud.Provisioner.Name(), "server_url", cloud.ServerURL)
	}
	runnerMgr := runners.New(db.Pool, svc, hub, log, runners.Config{
		Autopilot:      os.Getenv("GATOR_AUTOPILOT") != "0",
		DefaultBackend: os.Getenv("GATOR_DEFAULT_BACKEND"),
		Preparer:       pluginHost,
		Judge:          pluginHost,
		// An automation's work is its agent's: error triage has @triage on the incident.
		Authors: automations.Authors{},
		// What every job is handed of the Brain, and cites of it, is recorded.
		Brain: knows.Citations,
		// Every job gets an identity of its own to talk back with, revoked when it ends.
		AgentTokens: auth.Tokens{Pool: db.Pool},
		// A cloud job no connected runner can take gets a machine of its own, when configured.
		Provisioner: cloud.Provisioner, RunnerTokens: auth.Tokens{Pool: db.Pool},
		CloudServerURL: cloud.ServerURL, CloudEnv: cloud.Env,
	})

	go chat.Consumer(hub, log, 0).Run(ctx)
	// Agents are members: a mention, or a message in a DM with one, is a turn of that agent,
	// within the limits of a chain of delegations and the project's budget.
	team := agents.New(db.Pool, agents.Deps{Conversations: chat, Process: svc, Jobs: runnerMgr, Roles: auth.Authorizer{Pool: db.Pool}, Log: log})
	runnerMgr.UseBriefer(team.Briefer)
	// Agents outside Gator, through A2A: a remote agent's turn is a task sent to its card's
	// endpoint (through netguard), and the project's clients call the agents it exposes at /a2a.
	farAgents := a2a.NewRemote(a2a.RemoteDeps{Queries: dbq.New(db.Pool), Keys: keyring, Client: a2a.Client{HTTP: outbound.Client(2 * time.Minute)},
		Roles: auth.Authorizer{Pool: db.Pool}, Voice: team.Voice, Open: chat.Access, Packer: chat.Packer, Log: log})
	team.Starter.AnswerElsewhere(agents.KindRemote, farAgents)
	go farAgents.Run(ctx, 2*time.Second)
	a2aIn := a2a.NewInbound(a2a.InboundDeps{Pool: db.Pool, Bridge: bridge, Tokens: auth.Tokens{Pool: db.Pool},
		Roles: auth.Authorizer{Pool: db.Pool}, PublicURL: cfg.PublicURL})
	go team.Consumer(hub, log, 0).Run(ctx)
	go team.Streamer(hub, log, 0).Run(ctx)
	go team.Sweeper(log).Run(ctx, time.Minute)
	go runnerMgr.Run(ctx)
	if os.Getenv("GATOR_AUTOPILOT") != "0" {
		go runnerMgr.RunAutopilot(ctx)
		log.Info("autopilot on: entering a runner phase queues a job")
	}
	if chat.Attachments != nil {
		go chat.Attachments.RunSweeper(ctx, time.Hour, log)
	}
	// A mention, a DM or a reply in one's thread is pushed, to people who may read it.
	go notifier.WithChat(chat.Access).Run(ctx)

	// What a thread settled is proposed to the Brain once it goes quiet or its task closes.
	curate := curator.New(dbq.New(db.Pool), curator.PoolTx(db.Pool), team.Curations, chat.Packer, chat.Access, knows.Service, log)
	curations := conversations.NewCursorConsumer("brain_curator", db.Pool, hub, log, 0)
	curations.On(curate, curate.Events()...)
	go curations.Run(ctx)
	go curate.Run(ctx, curator.DefaultEvery)
	// A person's focus is worked out from their work, and theirs alone.
	go knows.Focus.Run(ctx, time.Hour)

	// Agents, skills, integrations and processes come from registries: Gator's own, fetched now so
	// a fresh server has a catalogue, and the company's. Nothing installed updates by itself.
	// Native plugins from a registry are built for this platform, checked against their sha256 and
	// kept next to the bundled ones, where the catalogue does not look.
	market := marketplace.New(db.Pool, marketplace.Deps{Capabilities: plugins.Capabilities{Pool: db.Pool},
		Plugins: pluginHost, PluginDir: marketplacePluginDir(pluginCfg.CatalogueDir),
		Net: outbound, AllowFileDownloads: os.Getenv("GATOR_MARKETPLACE_ALLOW_FILE") == "1"})
	go func() {
		if err := market.Start(ctx, marketplace.BuiltinFromEnv(os.Getenv)); err != nil && ctx.Err() == nil {
			log.Warn("marketplace: Gator's registry could not be fetched; the catalogue waits for a fetch", "err", err)
		}
	}()

	apiServer := &api.Server{
		Pool: db.Pool, Process: svc, Runners: runnerMgr, Plugins: pluginHost, Hub: hub, Log: log,
		Quality: scorecards, Automations: autos,
		Tokens: auth.Tokens{Pool: db.Pool}, Authz: auth.Authorizer{Pool: db.Pool}, DevAuth: cfg.DevAuth,
		RateLimitPerSecond: cfg.RateLimitPerSecond, RateLimitBurst: cfg.RateLimitBurst, TrustedProxies: cfg.TrustedProxies,
		PublicURL: cfg.PublicURL,
		// Channels, DMs and threads, with the same access rules for the API, the WebSocket and search.
		Conversations: chat,
		// The agents who answer in them.
		Agents: team,
		Brain:  knows,
		// Packages from registries, installed with the permissions a person accepted.
		Marketplace: market,
		// Remote MCP servers agents reach through the proxy; their credentials stay here.
		Remote: remote,
		// Links between people and their accounts on the bridges.
		Bridges: bridge,
		// Agents outside Gator, both ways.
		A2A: &a2a.Module{Remote: farAgents, Inbound: a2aIn},
		// The Brain and the agent surface as an MCP server, for jobs and for people's own clients.
		MCP: mcp.Standard(mcp.Deps{Brain: knows, Queries: dbq.New(db.Pool), Roles: auth.Authorizer{Pool: db.Pool},
			Talk: team.Talk, Tasks: svc, Log: log}),
	}
	oc := auth.LoadOIDCConfig()
	// A server people sign in to, or reach at a public address, is not a developer's laptop:
	// trusting X-Gator-User there lets anyone be anyone.
	if cfg.DevAuth && (oc.Enabled() || cfg.PublicURL != "") {
		return errors.New("GATOR_DEV_AUTH=1 is for local development; unset it on a server with GATOR_OIDC_* or GATOR_PUBLIC_URL")
	}
	if oc.Enabled() {
		o, err := auth.NewOIDC(ctx, oc, db.Pool)
		if err != nil {
			return err
		}
		apiServer.OIDC = o
		log.Info("oidc enabled", "issuer", oc.Issuer)
	} else {
		log.Warn("oidc not configured; only bearer tokens authenticate", "dev_auth", cfg.DevAuth)
	}
	if cfg.DevAuth {
		log.Warn("GATOR_DEV_AUTH=1: X-Gator-User header is trusted; never enable in production")
	}
	root := chi.NewRouter()
	root.Mount("/", apiServer.Router())
	// The only public endpoint: plugin webhooks. Plugins verify the sender's signature.
	root.Mount("/hooks", pluginHost.Hooks())
	// A2A: the cards of the agents projects expose, and their JSON-RPC endpoint for the projects'
	// clients, whose tokens reach nothing else.
	root.Mount(auth.A2APath, auth.Authenticate(apiServer.Tokens, cfg.DevAuth, log)(a2aIn.Handler()))
	// Where a provider sends the browser back after "Connect GitHub"; the state is the proof.
	// A browser signed in to Gator must be the person who started the connection.
	root.Mount("/connect", auth.Authenticate(apiServer.Tokens, cfg.DevAuth, log)(pluginHost.ConnectHandler()))
	adminRouter := chi.NewRouter()
	adminRouter.Use(auth.Authenticate(apiServer.Tokens, cfg.DevAuth, log), auth.RequireAuth, auth.RequireWorkspaceAdmin)
	adminRouter.Mount("/", (&admin.Handler{Pool: db.Pool, Process: svc}).Router())
	root.Mount("/admin", adminRouter)

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: root, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "version", version.Version)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// Stop taking HTTP first, then let in-flight queue jobs finish (soft stop), then hard stop.
		httpErr := srv.Shutdown(shutdownCtx)
		if err := queue.Stop(shutdownCtx); err != nil {
			log.Warn("queue soft stop timed out; forcing", "err", err)
			_ = queue.StopAndCancel(context.Background())
		}
		return httpErr
	}
}

// marketplacePluginDir is where native plugins from the marketplace go: GATOR_PLUGIN_DIR/marketplace,
// or nowhere when the server has no plugin directory.
func marketplacePluginDir(catalogue string) string {
	if catalogue == "" {
		return ""
	}
	return filepath.Join(catalogue, "marketplace")
}

// oauthApps are the server's OAuth apps for remote MCP servers, from the environment as plugins'
// are (GATOR_<PROVIDER>_CLIENT_ID, _CLIENT_SECRET, _SCOPES), each usable only at the authorization
// server hosts the operator lists for it (GATOR_<PROVIDER>_OAUTH_HOSTS); without them, remote
// servers register clients of their own.
func oauthApps(provider string) (mcpremote.OAuthApp, bool) {
	c, ok := plugins.OAuthClientFromEnv(provider)
	app := mcpremote.OAuthApp{ID: c.ID, Secret: c.Secret, Scopes: c.Scopes}
	for h := range strings.SplitSeq(os.Getenv("GATOR_"+strings.ToUpper(strings.ReplaceAll(provider, "-", "_"))+"_OAUTH_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			app.Hosts = append(app.Hosts, h)
		}
	}
	return app, ok && len(app.Hosts) > 0
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

// secretsCmd manages the encryption keyring.
//
//	secrets new-key [id]  print a fresh key for GATOR_SECRETS_KEY
//	secrets check         verify the configured keyring parses; report current key id
//
// Re-encryption of stored ciphertexts (rotate) is registered by the packages that own
// encrypted columns; the first one lands with plugin configuration in M3.
func secretsCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gator-server secrets <new-key [id]|check>")
	}
	switch args[0] {
	case "new-key":
		id := "k" + time.Now().UTC().Format("20060102")
		if len(args) > 1 {
			id = args[1]
		}
		fmt.Println(secrets.NewKey(id))
		return nil
	case "check":
		k, err := secrets.LoadKeyring()
		if err != nil {
			return err
		}
		fmt.Printf("keyring ok; current key id %s\n", k.CurrentID())
		return nil
	default:
		return fmt.Errorf("unknown secrets command %q", args[0])
	}
}

// adminCmd is the host-side break-glass for a fresh install: it needs database access,
// not an API token, so it works before OIDC is configured.
//
//	admin create-admin <email> [name]   create or promote a workspace admin; prints a 30-day token
//	admin issue-runner-token <name>     print a runner token
func adminCmd(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: gator-server admin <create-admin <email> [name]|issue-runner-token <name>>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	switch args[0] {
	case "create-admin":
		name := ""
		if len(args) > 2 {
			name = args[2]
		}
		u, token, err := auth.BootstrapAdmin(ctx, db.Pool, args[1], name, 30*24*time.Hour)
		if err != nil {
			return err
		}
		fmt.Printf("workspace admin: %s\n", u.Email)
		fmt.Printf("token (shown once, valid 30 days): %s\n", token)
		return nil
	case "issue-runner-token":
		token, err := auth.IssueRunnerToken(ctx, db.Pool, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("runner token for %s (shown once): %s\n", args[1], token)
		return nil
	default:
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}

// knowledgeCmd works with knowledge packs on disk.
//
//	knowledge checksum <pack dir>  print the checksum for a registry index
func knowledgeCmd(args []string) error {
	if len(args) < 2 || args[0] != "checksum" {
		return errors.New("usage: gator-server knowledge checksum <pack dir>")
	}
	pack, err := knowledge.ReadDir(args[1])
	if err != nil {
		return err
	}
	fmt.Println(pack.Checksum())
	return nil
}
