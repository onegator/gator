// gator-runner executes jobs handed out by gator-server. The same binary runs on a VPS
// under systemd and on a Mac under launchd.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/config"
	"github.com/onegator/gator/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gator-runner:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gator-runner <run|version>")
	}
	switch args[0] {
	case "version":
		fmt.Printf("gator-runner %s (%s, %s) proto=%d\n", version.Version, version.Commit, version.Date, proto.Version)
		return nil
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
		log.Info("runner configured", "name", cfg.Name, "location", cfg.Location, "server", cfg.ServerURL, "proto", proto.Version)
		// M2: connect to server, register, heartbeat, lease loop.
		<-ctx.Done()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
