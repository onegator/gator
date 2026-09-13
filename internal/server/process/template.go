// Package process owns tasks, phases, templates and the operations that move a task.
// Phases and templates are data: a new phase or kind is a YAML change, not a Go change.
package process

import (
	"embed"
	"fmt"
	"io/fs"
	"time"

	"gopkg.in/yaml.v3"
)

// Owner says who is expected to act in a phase.
type Owner string

const (
	OwnerHuman  Owner = "human"
	OwnerRunner Owner = "runner"
	OwnerPlugin Owner = "plugin"
)

// GateKind says what must be satisfied to leave a phase.
type GateKind string

const (
	GateHuman GateKind = "human" // a person approves
	GateAuto  GateKind = "auto"  // automated checks only (plugins, timeouts)
	GateBoth  GateKind = "both"  // automated checks pass AND a person approves
)

// Phase is one step of a template.
type Phase struct {
	Name     string   `yaml:"name"`
	Owner    Owner    `yaml:"owner"`
	Role     string   `yaml:"role,omitempty"` // runner role when Owner == runner
	Gate     GateKind `yaml:"gate"`
	Timeout  Duration `yaml:"timeout"`
	Requires string   `yaml:"requires,omitempty"` // plugin capability that activates this phase
}

// Duration is a time.Duration that accepts "24h" and a bare 0 in YAML.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Value == "0" || n.Value == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("timeout %q: %w", n.Value, err)
	}
	*d = Duration(v)
	return nil
}

// Std converts to time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Template is the ordered list of phases for one task kind.
type Template struct {
	Kind         string  `yaml:"kind"`
	MaxRollbacks int     `yaml:"max_rollbacks"`
	Phases       []Phase `yaml:"phases"`
}

// Validate checks structural rules every template must satisfy.
func (t Template) Validate() error {
	if t.Kind == "" {
		return fmt.Errorf("template without kind")
	}
	if len(t.Phases) == 0 {
		return fmt.Errorf("template %q has no phases", t.Kind)
	}
	seen := map[string]bool{}
	for i, p := range t.Phases {
		if p.Name == "" {
			return fmt.Errorf("template %q phase %d has no name", t.Kind, i)
		}
		if seen[p.Name] {
			return fmt.Errorf("template %q repeats phase %q", t.Kind, p.Name)
		}
		seen[p.Name] = true
		switch p.Owner {
		case OwnerHuman, OwnerRunner, OwnerPlugin:
		default:
			return fmt.Errorf("template %q phase %q has unknown owner %q", t.Kind, p.Name, p.Owner)
		}
		if p.Owner == OwnerRunner && p.Role == "" {
			return fmt.Errorf("template %q phase %q is runner-owned but has no role", t.Kind, p.Name)
		}
		switch p.Gate {
		case GateHuman, GateAuto, GateBoth:
		default:
			return fmt.Errorf("template %q phase %q has unknown gate %q", t.Kind, p.Name, p.Gate)
		}
	}
	return nil
}

// Catalog holds every known template by kind.
type Catalog map[string]Template

//go:embed all:defaults
var defaultsFS embed.FS

// DefaultCatalog loads the templates shipped with the binary.
func DefaultCatalog() (Catalog, error) {
	sub, err := fs.Sub(defaultsFS, "defaults")
	if err != nil {
		return nil, err
	}
	return LoadCatalog(sub)
}

// LoadCatalog reads every *.yaml in the given filesystem.
func LoadCatalog(fsys fs.FS) (Catalog, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	c := Catalog{}
	for _, e := range entries {
		if e.IsDir() || len(e.Name()) < 6 || e.Name()[len(e.Name())-5:] != ".yaml" {
			continue
		}
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		var t Template
		if err := yaml.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		c[t.Kind] = t
	}
	return c, nil
}

// Override applies per-project overrides on top of the catalog. Overrides are full
// templates keyed by kind; a kind absent from overrides keeps the default.
func (c Catalog) Override(overrides Catalog) Catalog {
	out := Catalog{}
	for k, v := range c {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}
