package mcpremote

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/onegator/gator/internal/server/netguard"
	"github.com/onegator/gator/internal/server/secrets"
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
	for k, vs := range u.Query() {
		lk := strings.ToLower(k)
		if secrets.IsSensitiveKey(k) || lk == "key" || lk == "auth" || lk == "sig" || lk == "signature" || slices.ContainsFunc(vs, secrets.LooksSecret) {
			// An address is shown to the project and written in records; a credential belongs in
			// the server's secrets, which only the proxy reads.
			return fmt.Errorf("%w: %q carries a credential in its query (%s); set it as the server's token instead", ErrInvalid, u.Redacted(), k)
		}
	}
	return nil
}
