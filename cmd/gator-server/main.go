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

	"github.com/onegator/gator/internal/server/config"
	"github.com/onegator/gator/internal/server/httpapi"
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

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpapi.NewRouter(db),
		ReadHeaderTimeout: 10 * time.Second,
	}
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
