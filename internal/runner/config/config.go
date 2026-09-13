// Package config loads gator-runner configuration from the environment.
package config

import (
	"errors"
	"os"
)

// Config is the runner configuration.
type Config struct {
	ServerURL string // wss://gator.example.com/runner
	Token     string
	Name      string
	Location  string // "vps" | "mac"
	WorkDir   string // where worktrees live
}

// Load reads GATOR_RUNNER_* variables.
func Load() (Config, error) {
	host, _ := os.Hostname()
	c := Config{
		ServerURL: os.Getenv("GATOR_RUNNER_SERVER_URL"),
		Token:     os.Getenv("GATOR_RUNNER_TOKEN"),
		Name:      env("GATOR_RUNNER_NAME", host),
		Location:  env("GATOR_RUNNER_LOCATION", "vps"),
		WorkDir:   env("GATOR_RUNNER_WORKDIR", os.ExpandEnv("$HOME/.gator/work")),
	}
	if c.ServerURL == "" {
		return c, errors.New("GATOR_RUNNER_SERVER_URL is required")
	}
	if c.Token == "" {
		return c, errors.New("GATOR_RUNNER_TOKEN is required")
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
