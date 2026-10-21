// Package config loads gator-runner configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/onegator/gator/internal/proto"
)

// Config is the runner configuration.
type Config struct {
	ServerURL string // https://gator.<tailnet>.ts.net; the protocol path is appended
	Token     string
	Name      string
	Location  string // "vps" | "mac"
	// Placement is which jobs this runner takes: "local" (jobs people run on their own
	// machines) or "cloud". GATOR_RUNNER_PLACEMENT; empty derives it from Location.
	Placement   string
	WorkDir     string // where worktrees live
	Backends    []string
	Projects    []string
	MaxParallel int
	// Once makes the runner exit after its first job: a machine started for one job.
	Once bool
	// TokenFile is where the token was read from, so the agent can be kept from reading it.
	TokenFile string
	// AgentEnv names variables the agent gets on top of the built-in allow-list
	// (GATOR_RUNNER_AGENT_ENV), for a backend or a tool that needs one this runner cannot guess.
	AgentEnv []string
}

// Load reads GATOR_RUNNER_* variables.
func Load() (Config, error) {
	host, _ := os.Hostname()
	c := Config{
		ServerURL:   os.Getenv("GATOR_RUNNER_SERVER_URL"),
		Token:       os.Getenv("GATOR_RUNNER_TOKEN"),
		Name:        env("GATOR_RUNNER_NAME", host),
		Location:    env("GATOR_RUNNER_LOCATION", "vps"),
		WorkDir:     env("GATOR_RUNNER_WORKDIR", os.ExpandEnv("$HOME/.gator/work")),
		Backends:    list(os.Getenv("GATOR_RUNNER_BACKENDS")),
		Projects:    list(os.Getenv("GATOR_RUNNER_PROJECTS")),
		MaxParallel: 1,
		Placement:   os.Getenv("GATOR_RUNNER_PLACEMENT"),
		Once:        os.Getenv("GATOR_RUNNER_ONCE") == "1",
		AgentEnv:    list(os.Getenv("GATOR_RUNNER_AGENT_ENV")),
	}
	if c.Placement == "" {
		c.Placement = proto.PlacementFor(c.Location)
	}
	if !proto.ValidPlacement(c.Placement) {
		return c, fmt.Errorf("GATOR_RUNNER_PLACEMENT must be local or cloud, not %q", c.Placement)
	}
	// A slug or a typo used to be dropped on the server, and a list of nothing but those
	// meant "any project" — the opposite of what was written.
	for _, p := range c.Projects {
		if _, err := uuid.Parse(p); err != nil {
			return c, fmt.Errorf("GATOR_RUNNER_PROJECTS takes project ids; %q is not one", p)
		}
	}
	if v, err := strconv.Atoi(os.Getenv("GATOR_RUNNER_MAX_PARALLEL")); err == nil && v > 0 {
		c.MaxParallel = v
	}
	// Handed over by the runner itself, re-executed without the variable (see shedToken).
	if fd := os.Getenv("GATOR_RUNNER_TOKEN_FD"); fd != "" && c.Token == "" {
		n, err := strconv.Atoi(fd)
		if err != nil {
			return c, fmt.Errorf("GATOR_RUNNER_TOKEN_FD: %w", err)
		}
		f := os.NewFile(uintptr(n), "runner-token")
		b, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return c, fmt.Errorf("GATOR_RUNNER_TOKEN_FD: %w", err)
		}
		c.Token = strings.TrimSpace(string(b))
	}
	// A launchd plist is read by anyone who can list the home directory, and a unit file gets
	// copied around. Pointing at a file keeps the token in one place with a mode of its own.
	if path := os.Getenv("GATOR_RUNNER_TOKEN_FILE"); path != "" && c.Token == "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("GATOR_RUNNER_TOKEN_FILE: %w", err)
		}
		c.Token = strings.TrimSpace(string(b))
		c.TokenFile = path
	}
	if c.ServerURL == "" {
		return c, errors.New("GATOR_RUNNER_SERVER_URL is required")
	}
	if c.Token == "" {
		return c, errors.New("GATOR_RUNNER_TOKEN or GATOR_RUNNER_TOKEN_FILE is required")
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func list(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
