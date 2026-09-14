// Package config loads gator-runner configuration from the environment.
package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Config is the runner configuration.
type Config struct {
	ServerURL   string // https://gator.<tailnet>.ts.net; the protocol path is appended
	Token       string
	Name        string
	Location    string // "vps" | "mac"
	WorkDir     string // where worktrees live
	Backends    []string
	Projects    []string
	MaxParallel int
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
	}
	if v, err := strconv.Atoi(os.Getenv("GATOR_RUNNER_MAX_PARALLEL")); err == nil && v > 0 {
		c.MaxParallel = v
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

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
