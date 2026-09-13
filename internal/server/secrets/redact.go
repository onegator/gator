package secrets

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
)

// Redacted replaces secret values in logs.
const Redacted = "[redacted]"

var sensitiveKeys = []string{"secret", "token", "password", "passwd", "api_key", "apikey", "authorization", "cookie", "private_key", "client_secret"}

// IsSensitiveKey reports whether a field name looks like it carries a secret.
func IsSensitiveKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// RedactMap returns a copy with sensitive keys masked, recursing into nested maps.
func RedactMap(m map[string]any, extra ...string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if IsSensitiveKey(k) || contains(extra, k) {
			out[k] = Redacted
			continue
		}
		if nested, ok := v.(map[string]any); ok {
			out[k] = RedactMap(nested, extra...)
			continue
		}
		out[k] = v
	}
	return out
}

// RedactJSON masks sensitive keys in a JSON object; non-objects pass through.
func RedactJSON(b []byte, extra ...string) []byte {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return b
	}
	out, err := json.Marshal(RedactMap(m, extra...))
	if err != nil {
		return b
	}
	return out
}

// RedactingHandler wraps a slog.Handler and masks attributes whose key looks sensitive.
type RedactingHandler struct{ Inner slog.Handler }

// Enabled implements slog.Handler.
func (h RedactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.Inner.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(redactAttr(a))
		return true
	})
	return h.Inner.Handle(ctx, nr)
}

// WithAttrs implements slog.Handler.
func (h RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, redactAttr(a))
	}
	return RedactingHandler{Inner: h.Inner.WithAttrs(out)}
}

// WithGroup implements slog.Handler.
func (h RedactingHandler) WithGroup(name string) slog.Handler {
	return RedactingHandler{Inner: h.Inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	if a.Value.Kind() == slog.KindGroup {
		g := a.Value.Group()
		out := make([]slog.Attr, 0, len(g))
		for _, x := range g {
			out = append(out, redactAttr(x))
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	return a
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
