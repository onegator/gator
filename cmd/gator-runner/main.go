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

	"path/filepath"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/backend/claude"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/runner/config"
	"github.com/onegator/gator/internal/runner/executor"
	"github.com/onegator/gator/internal/runner/workspace"
	"github.com/onegator/gator/internal/version"
)

// agentCLI finds gator-cli: next to this binary, which is how every release and the Mac app
// ship it, or wherever GATOR_RUNNER_CLI_PATH says. Without it the agent gets no tools and the
// prompt does not pretend otherwise.
func agentCLI(log *slog.Logger) string {
	if p := os.Getenv("GATOR_RUNNER_CLI_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		log.Warn("GATOR_RUNNER_CLI_PATH does not exist; jobs run without agent tools", "path", p)
		return ""
	}
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(self), "gator-cli")
	if _, err := os.Stat(candidate); err != nil {
		log.Info("no gator-cli next to the runner; jobs run without agent tools", "looked", candidate)
		return ""
	}
	return candidate
}

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
		backends := map[string]backend.Backend{}
		var advertised []string
		for _, name := range cfg.Backends {
			switch name {
			case "claude":
				backends[name] = claude.Backend{Bin: os.Getenv("GATOR_RUNNER_CLAUDE_BIN"), Model: os.Getenv("GATOR_RUNNER_CLAUDE_MODEL"),
					MCPConfig: os.Getenv("GATOR_RUNNER_CLAUDE_MCP_CONFIG"), Log: log}
				advertised = append(advertised, name)
			default:
				log.Warn("unknown backend ignored", "backend", name)
			}
		}
		if len(advertised) == 0 {
			log.Warn("no usable GATOR_RUNNER_BACKENDS; this runner stays online but receives no jobs")
		}
		ex := &executor.Executor{
			WS:        &workspace.Manager{Root: cfg.WorkDir},
			Backends:  backends,
			Push:      os.Getenv("GATOR_RUNNER_PUSH") != "0",
			Log:       log,
			ServerURL: cfg.ServerURL,
			CLIPath:   agentCLI(log),
		}
		for name, state := range ex.AuthState() {
			log.Info("backend", "name", name, "auth", state)
		}
		c := client.New(client.Config{
			ServerURL: cfg.ServerURL, Token: cfg.Token, Name: cfg.Name, Location: cfg.Location,
			BinaryVersion: version.Version, Backends: advertised, Projects: cfg.Projects, MaxParallel: cfg.MaxParallel,
			JournalPath: filepath.Join(cfg.WorkDir, "outbox.json"),
		}, ex, log)
		log.Info("runner starting", "name", cfg.Name, "location", cfg.Location, "server", cfg.ServerURL, "proto", proto.Version)
		return c.Run(ctx)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
