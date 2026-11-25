package mcpremote

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
)

// Auth of a remote server.
const (
	AuthOAuth  = "oauth"
	AuthBearer = "bearer"
	AuthNone   = "none"
)

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	toolPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
)

// ValidName says whether s can name a remote server: it is part of a URL and of its tools' names
// in a session (mcp__<name>__<tool>), and gator is Gator's own.
func ValidName(s string) bool { return namePattern.MatchString(s) && s != "gator" }

// ValidToolName says whether s can name a remote tool.
func ValidToolName(s string) bool { return toolPattern.MatchString(s) }

// CheckURL holds an address the server will send credentials to: https, or plain http to this
// machine only (a server on localhost, a test).
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%w: %q is not a server's address", ErrInvalid, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if Loopback(u.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q must be https (plain http only to localhost)", ErrInvalid, raw)
}

// Loopback says whether host is this machine.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
