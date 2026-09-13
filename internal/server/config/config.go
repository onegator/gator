// Package config loads gator-server configuration from the environment.
package config

import (
	"errors"
	"os"
	"strconv"
)

// Config is the complete server configuration. Every field has a working default
// except DatabaseURL, which is required.
type Config struct {
	ListenAddr  string
	DatabaseURL string
	LogLevel    string
	DevAuth     bool // GATOR_DEV_AUTH=1 accepts X-Gator-User; local development only
}

// Load reads configuration from environment variables prefixed GATOR_.
func Load() (Config, error) {
	c := Config{
		ListenAddr:  env("GATOR_LISTEN_ADDR", ":8080"),
		DatabaseURL: os.Getenv("GATOR_DATABASE_URL"),
		LogLevel:    env("GATOR_LOG_LEVEL", "info"),
		DevAuth:     os.Getenv("GATOR_DEV_AUTH") == "1",
	}
	if c.DatabaseURL == "" {
		return c, errors.New("GATOR_DATABASE_URL is required")
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// EnvInt reads an integer from the environment with a fallback.
func EnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
