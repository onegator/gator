// Package redact masks known secret values in text. The server and the runner both mask a job's
// login in what they keep and send, with this one rule, so neither can drift from the other; it
// imports nothing of either.
package redact

import "strings"

// Redacted replaces a secret value.
const Redacted = "[redacted]"

// MinLen is the shortest value masked: a value shorter than that is not a secret, and masking
// "ok" everywhere would hide more than it protects.
const MinLen = 8

// Values replaces each of values found in s with Redacted. Tokens are written without characters
// JSON escapes, so masking a JSON document's text masks them inside its strings too.
func Values(s string, values []string) string {
	for _, v := range values {
		if len(v) >= MinLen {
			s = strings.ReplaceAll(s, v, Redacted)
		}
	}
	return s
}

// Union is a and b without repeats, a's order first.
func Union(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	seen := map[string]bool{}
	for _, v := range append(append([]string{}, a...), b...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
