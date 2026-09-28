// Package config loads gator-server configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/onegator/gator/internal/server/limits"
)

// Config is the complete server configuration. Every field has a working default
// except DatabaseURL, which is required.
type Config struct {
	ListenAddr         string
	DatabaseURL        string
	LogLevel           string
	DevAuth            bool    // GATOR_DEV_AUTH=1 accepts X-Gator-User; local development only
	RateLimitPerSecond float64 // GATOR_RATE_LIMIT_RPS, default 20; 0 disables
	RateLimitBurst     int     // GATOR_RATE_LIMIT_BURST, default 40
	// TrustedProxies may name the client in X-Forwarded-For (GATOR_TRUSTED_PROXIES, a comma-
	// separated list of addresses and CIDRs, default loopback). Nobody else can.
	TrustedProxies limits.Proxies
}

// DefaultTrustedProxies is loopback: Caddy and tailscale serve both proxy from this host.
const DefaultTrustedProxies = "127.0.0.0/8,::1"

// Load reads configuration from environment variables prefixed GATOR_.
func Load() (Config, error) {
	c := Config{
		ListenAddr:         env("GATOR_LISTEN_ADDR", ":8080"),
		DatabaseURL:        os.Getenv("GATOR_DATABASE_URL"),
		LogLevel:           env("GATOR_LOG_LEVEL", "info"),
		DevAuth:            os.Getenv("GATOR_DEV_AUTH") == "1",
		RateLimitPerSecond: envFloat("GATOR_RATE_LIMIT_RPS", 20),
		RateLimitBurst:     EnvInt("GATOR_RATE_LIMIT_BURST", 40),
	}
	if c.DatabaseURL == "" {
		return c, errors.New("GATOR_DATABASE_URL is required")
	}
	proxies, err := limits.ParseProxies(env("GATOR_TRUSTED_PROXIES", DefaultTrustedProxies))
	if err != nil {
		return c, fmt.Errorf("GATOR_TRUSTED_PROXIES: %w", err)
	}
	c.TrustedProxies = proxies
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

func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}
