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
	"strings"
	"syscall"

	"path/filepath"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/agentenv"
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

// agentDenyRead is the files the agent must not open: the token file this runner read. On a
// Mac the sandbox enforces it; elsewhere the runner says plainly that it cannot.
// GATOR_RUNNER_SANDBOX=0 turns the sandbox off, for a backend that will not run inside one.
func agentDenyRead(cfg config.Config, log *slog.Logger) []string {
	if cfg.TokenFile == "" {
		return nil
	}
	if abs, err := filepath.Abs(cfg.TokenFile); err == nil {
		if work, err := filepath.Abs(cfg.WorkDir); err == nil && strings.HasPrefix(abs, work+string(os.PathSeparator)) {
			log.Warn("the token file is inside the work directory, where agents work; move it out", "token_file", abs, "workdir", work)
		}
	}
	if os.Getenv("GATOR_RUNNER_SANDBOX") == "0" {
		log.Warn("GATOR_RUNNER_SANDBOX=0: agents can read the token file")
		return nil
	}
	if !agentenv.Sandboxed() {
		log.Info("no sandbox on this platform: agents run as this user and could read the token file; see docs/deploy.md",
			"token_file", cfg.TokenFile)
	}
	return []string{cfg.TokenFile}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gator-runner:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gator-runner <run [--once]|version>")
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Printf("gator-runner %s (%s, %s) proto=%d\n", version.Version, version.Commit, version.Date, proto.Version)
		return nil
	case "run":
		if err := shedToken(); err != nil {
			fmt.Fprintln(os.Stderr, "gator-runner: could not take the token out of the environment:", err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		for _, a := range args[1:] {
			switch a {
			case "--once":
				cfg.Once = true
			default:
				return fmt.Errorf("unknown flag %q for run", a)
			}
		}
		log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
		denyRead := agentDenyRead(cfg, log)
		backends := map[string]backend.Backend{}
		var advertised []string
		for _, name := range cfg.Backends {
			switch name {
			case "claude":
				backends[name] = claude.Backend{Bin: os.Getenv("GATOR_RUNNER_CLAUDE_BIN"), Model: os.Getenv("GATOR_RUNNER_CLAUDE_MODEL"),
					MCPConfig: os.Getenv("GATOR_RUNNER_CLAUDE_MCP_CONFIG"), Log: log, AllowEnv: cfg.AgentEnv}
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
			DenyRead:  denyRead,
		}
		for name, state := range ex.AuthState() {
			log.Info("backend", "name", name, "auth", state)
		}
		c := client.New(client.Config{
			ServerURL: cfg.ServerURL, Token: cfg.Token, Name: cfg.Name, Location: cfg.Location, Placement: cfg.Placement, Once: cfg.Once,
			BinaryVersion: version.Version, Backends: advertised, Projects: cfg.Projects, MaxParallel: cfg.MaxParallel,
			JournalPath: filepath.Join(cfg.WorkDir, "outbox.json"),
		}, ex, log)
		log.Info("runner starting", "name", cfg.Name, "location", cfg.Location, "placement", cfg.Placement, "once", cfg.Once,
			"server", cfg.ServerURL, "proto", proto.Version)
		return c.Run(ctx)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
