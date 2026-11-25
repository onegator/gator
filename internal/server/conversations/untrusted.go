package conversations

import (
	"fmt"
	"strings"

	"github.com/onegator/gator/internal/server/untrusted"
)

// Marker fences words written outside the workspace in a prompt (untrusted.Marker), and renders a
// pack's messages with it.
type Marker struct {
	untrusted.Marker
}

// NewMarker makes a marker with a fresh nonce.
func NewMarker() Marker { return Marker{untrusted.New()} }

// Render is one message as a prompt carries it: a member's words as they are, outside words fenced.
func (m Marker) Render(s Said) string {
	if s.Untrusted {
		return m.Fence(s.Who, s.Body)
	}
	return fmt.Sprintf("%s: %s", s.Who, strings.TrimSpace(s.Body))
}
