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
	"github.com/onegator/gator/internal/server/store"
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
		return errors.New("usage: gator-server <serve|migrate|migrate-down|version>")
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
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))
	slog.SetDefault(log)

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
