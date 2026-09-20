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
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/onegator/gator/internal/server/admin"
	"github.com/onegator/gator/internal/server/api"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/backup"
	"github.com/onegator/gator/internal/server/config"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/jobs"
	"github.com/onegator/gator/internal/server/knowledge"
	"github.com/onegator/gator/internal/server/notify"
	"github.com/onegator/gator/internal/server/plugins"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/product"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store"
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
		return errors.New("usage: gator-server <serve|migrate|migrate-down|admin create-admin <email> [name]|admin issue-runner-token <name>|backup now|restore <file>|secrets new-key [id]|secrets check|version>")
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
	relay := &events.Relay{Pool: db.Pool, Hub: hub, Log: log}
	go relay.Run(ctx)
	pluginHost := plugins.New(db.Pool, svc, keyring, hub, log, plugins.Config{})
	go pluginHost.Run(ctx)
	// Push notifications: without an APNs key the sender is disabled and nothing leaves here.
	var sender notify.Sender = notify.Disabled{}
	if apns, err := notify.LoadAPNs(); err != nil {
		return err
	} else if apns != nil {
		sender = apns
	}
	go notify.New(db.Pool, svc, sender, hub, log, 0).Run(ctx)
	// Finished work proposes what the product learned; a person approves it.
	go product.NewCurator(db.Pool, hub, log, 0).Run(ctx)

	bc := backup.LoadConfig()
	queue, err := jobs.New(jobs.Options{Pool: db.Pool, Process: svc, Backup: bc, DatabaseURL: cfg.DatabaseURL,
		Plugins: pluginHost, Releases: pluginHost, Log: log})
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

	runnerMgr := runners.New(db.Pool, svc, hub, log, runners.Config{
		Autopilot:      os.Getenv("GATOR_AUTOPILOT") != "0",
		DefaultBackend: os.Getenv("GATOR_DEFAULT_BACKEND"),
		Preparer:       pluginHost,
	})
	go runnerMgr.Run(ctx)
	if os.Getenv("GATOR_AUTOPILOT") != "0" {
		go runnerMgr.RunAutopilot(ctx)
		log.Info("autopilot on: entering a runner phase queues a job")
	}

	apiServer := &api.Server{
		Pool: db.Pool, Process: svc, Runners: runnerMgr, Plugins: pluginHost, Hub: hub, Log: log,
		Tokens: auth.Tokens{Pool: db.Pool}, Authz: auth.Authorizer{Pool: db.Pool}, DevAuth: cfg.DevAuth,
		RateLimitPerSecond: cfg.RateLimitPerSecond, RateLimitBurst: cfg.RateLimitBurst,
	}
	if oc := auth.LoadOIDCConfig(); oc.Enabled() {
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
