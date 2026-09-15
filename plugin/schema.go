package plugin

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ConfigSchema is the subset of JSON Schema that plugin configuration uses: one object of
// typed properties. A property with "x-secret": true (or "writeOnly": true) is a secret: the
// core encrypts it, never returns or logs it, and passes it only to that project's process
// as GATOR_SECRET_<NAME>.
type ConfigSchema struct {
	Type       string                `json:"type"`
	Properties map[string]SchemaProp `json:"properties"`
	Required   []string              `json:"required"`
}

// SchemaProp is one configuration field.
type SchemaProp struct {
	Type        string `json:"type"` // string | number | integer | boolean | array | object
	Description string `json:"description,omitempty"`
	Secret      bool   `json:"x-secret,omitempty"`
	WriteOnly   bool   `json:"writeOnly,omitempty"`
	Enum        []any  `json:"enum,omitempty"`
}

// ParseConfigSchema reads a manifest's config_schema; empty means no configuration.
func ParseConfigSchema(raw json.RawMessage) (ConfigSchema, error) {
	var s ConfigSchema
	if len(raw) == 0 || string(raw) == "null" {
		return ConfigSchema{Type: "object"}, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("config_schema: %w", err)
	}
	if s.Type != "" && s.Type != "object" {
		return s, fmt.Errorf("config_schema: top-level type must be object")
	}
	for name, p := range s.Properties {
		switch p.Type {
		case "string", "number", "integer", "boolean", "array", "object":
		default:
			return s, fmt.Errorf("config_schema: property %q has unsupported type %q", name, p.Type)
		}
		if p.IsSecret() && p.Type != "string" {
			return s, fmt.Errorf("config_schema: secret %q must be a string", name)
		}
	}
	for _, r := range s.Required {
		if _, ok := s.Properties[r]; !ok {
			return s, fmt.Errorf("config_schema: required %q is not a property", r)
		}
	}
	return s, nil
}

// IsSecret reports whether the property holds a secret.
func (p SchemaProp) IsSecret() bool { return p.Secret || p.WriteOnly }

// IsSecret reports whether key is a secret property.
func (s ConfigSchema) IsSecret(key string) bool { return s.Properties[key].IsSecret() }

// SecretKeys lists the secret properties, sorted.
func (s ConfigSchema) SecretKeys() []string {
	var out []string
	for k, p := range s.Properties {
		if p.IsSecret() {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Validate checks a configuration: known keys only, required present, types and enums.
func (s ConfigSchema) Validate(cfg map[string]any) error {
	var problems []string
	for k, v := range cfg {
		p, ok := s.Properties[k]
		if !ok {
			problems = append(problems, fmt.Sprintf("unknown setting %q", k))
			continue
		}
		if !typeMatches(p.Type, v) {
			problems = append(problems, fmt.Sprintf("%q must be %s", k, p.Type))
			continue
		}
		if len(p.Enum) > 0 && !inEnum(p.Enum, v) {
			problems = append(problems, fmt.Sprintf("%q must be one of %v", k, p.Enum))
		}
	}
	for _, r := range s.Required {
		if v, ok := cfg[r]; !ok || v == nil || v == "" {
			problems = append(problems, fmt.Sprintf("%q is required", r))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Split separates secrets from the rest of a configuration.
func (s ConfigSchema) Split(cfg map[string]any) (public map[string]any, secrets map[string]string) {
	public, secrets = map[string]any{}, map[string]string{}
	for k, v := range cfg {
		if s.IsSecret(k) {
			if str, ok := v.(string); ok && str != "" {
				secrets[k] = str
			}
			continue
		}
		public[k] = v
	}
	return public, secrets
}

func typeMatches(t string, v any) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return false
}

func inEnum(enum []any, v any) bool {
	for _, e := range enum {
		if fmt.Sprint(e) == fmt.Sprint(v) {
			return true
		}
	}
	return false
}

// SecretEnv is the environment variable that carries a secret setting.
func SecretEnv(key string) string {
	var b strings.Builder
	b.WriteString("GATOR_SECRET_")
	for _, r := range strings.ToUpper(key) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// CoreSatisfies reports whether a core version meets a plugin's min_core_version. Development
// and snapshot builds satisfy everything.
func CoreSatisfies(core, min string) bool {
	if min == "" || core == "dev" || strings.Contains(core, "SNAPSHOT") {
		return true
	}
	c, ok1 := semver(core)
	m, ok2 := semver(min)
	if !ok1 || !ok2 {
		return true
	}
	for i := range c {
		if c[i] != m[i] {
			return c[i] > m[i]
		}
	}
	return true
}

func semver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
