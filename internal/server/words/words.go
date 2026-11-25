// Package words keeps what Gator holds of words and ids that come from outside — a Slack name, an
// Agent Card's description, an id in a request — to one shape everywhere: one line, bounded, no
// control characters.
package words

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

// OneLine is s on one line, its spaces collapsed, at most n characters.
func OneLine(s string, n int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
}

// Clip is s cut to n characters, saying so with an ellipsis.
func Clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// UUID reads an id; ok is false for anything that is not one.
func UUID(s string) (pgtype.UUID, bool) {
	var u pgtype.UUID
	return u, u.Scan(s) == nil && u.Valid
}

// UUIDString writes an id the way the API does; the zero id is "".
func UUIDString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
