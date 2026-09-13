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
	"github.com/onegator/gator/internal/server/config"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
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
		return errors.New("usage: gator-server <serve|migrate|migrate-down|secrets new-key [id]|secrets check|version>")
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
	case "secrets":
		return secretsCmd(args[1:])
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

	if _, err := secrets.LoadKeyring(); err != nil {
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
	// Capabilities come from enabled plugins once PLQ-225 lands; until then none.
	svc := process.NewService(db.Pool, process.DBTemplates{Pool: db.Pool, Defaults: catalog}, process.StaticCapabilities(nil))
	hub := events.NewHub()
	relay := &events.Relay{Pool: db.Pool, Hub: hub, Log: log}
	go relay.Run(ctx)

	apiServer := &api.Server{
		Pool: db.Pool, Process: svc, Hub: hub, Log: log,
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
		return srv.Shutdown(shutdownCtx)
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
