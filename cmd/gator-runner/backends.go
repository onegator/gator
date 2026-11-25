package main

import (
	"log/slog"
	"os"
	"slices"

	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/backend/acp"
	"github.com/onegator/gator/internal/runner/backend/claude"
	"github.com/onegator/gator/internal/runner/config"
)

// builtin makes one backend GATOR_RUNNER_BACKENDS can name. A new CLI with its own protocol is a
// new entry here; an agent that speaks ACP needs none (GATOR_ACP_AGENTS).
type builtin func(cfg config.Config, log *slog.Logger) (backend.Backend, error)

var builtins = map[string]builtin{
	"claude": func(cfg config.Config, log *slog.Logger) (backend.Backend, error) {
		// What a run that died left of a job's MCP configuration (its token) goes now.
		if err := claude.PrepareMCPDir(claude.DefaultMCPDir()); err != nil {
			return nil, err
		}
		return claude.Backend{Bin: os.Getenv("GATOR_RUNNER_CLAUDE_BIN"), Model: os.Getenv("GATOR_RUNNER_CLAUDE_MODEL"),
			MCPConfig: os.Getenv("GATOR_RUNNER_CLAUDE_MCP_CONFIG"), Log: log, AllowEnv: cfg.AgentEnv}, nil
	},
}

// backends are the backends this runner runs and advertises: the built-in ones it is told to
// run, and every ACP agent it is given, as acp:<name>. A name it knows nothing of is said and
// left out — the server then never hands it a job for that backend.
func backends(cfg config.Config, log *slog.Logger) (map[string]backend.Backend, []string, error) {
	out := map[string]backend.Backend{}
	var advertised []string
	for _, a := range cfg.ACPAgents {
		b := acp.New(a, cfg.AgentEnv, log)
		if b.Profile.Experimental {
			log.Warn("ACP agent without a recorded profile runs as experimental", "backend", b.Name(), "known", acp.Known())
		}
		out[b.Name()] = b
		advertised = append(advertised, b.Name())
	}
	for _, name := range cfg.Backends {
		if _, ok := out[name]; ok {
			continue // an ACP agent listed in both places is one backend
		}
		build, ok := builtins[name]
		if !ok {
			log.Warn("unknown backend ignored", "backend", name)
			continue
		}
		b, err := build(cfg, log)
		if err != nil {
			return nil, nil, err
		}
		out[name] = b
		advertised = append(advertised, name)
	}
	slices.Sort(advertised)
	return out, advertised, nil
}
