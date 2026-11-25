package mcpremote

import (
	"fmt"
	"net/url"
	"regexp"

	"github.com/onegator/gator/internal/server/netguard"
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
// machine where the policy allows private addresses (development, tests). Which addresses a name
// may resolve to is the policy's dialer's to check, when the connection is made.
func CheckURL(raw string, p netguard.Policy) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%w: %q is not a server's address", ErrInvalid, raw)
	}
	if err := p.CheckScheme(u); err != nil {
		return fmt.Errorf("%w: %v (plain http only to localhost, in development)", ErrInvalid, err)
	}
	return nil
}
