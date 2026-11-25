package marketplace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is what a package says about itself, the same for every kind: what the catalogue shows
// and what installing it asks a person to accept.
type Manifest struct {
	Kind          string   `json:"kind"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	DisplayName   string   `json:"display_name"`
	Description   string   `json:"description"`
	Phases        []string `json:"phases,omitempty"` // where in the life of software it helps: discovery … monitoring
	Icon          string   `json:"icon,omitempty"`   // a file of the package, or an SF Symbol
	ExamplePrompt string   `json:"example_prompt,omitempty"`
	Readme        string   `json:"readme,omitempty"`
	// Permissions are what installing it lets it do, in words a person accepts one by one. They
	// are exactly what is compared when a new version comes: one more line is one more thing to
	// accept again.
	Permissions []string `json:"permissions"`
	Requires    Requires `json:"requires"`
	// Detail is the kind's own manifest, as the package wrote it.
	Detail json.RawMessage `json:"detail,omitempty"`
}

// Requires is what a package needs of the project it goes into.
type Requires struct {
	Capabilities []string `json:"capabilities,omitempty"` // a plugin's capability: vcs, ci, deploy, monitoring …
	MCP          []string `json:"mcp,omitempty"`          // remote MCP servers by name
	Skills       []string `json:"skills,omitempty"`       // skills the package does not bring itself
}

// Package is one verified package of a registry: what its index said, what it says about itself
// and its files.
type Package struct {
	Item     Item     `json:"item"`
	Manifest Manifest `json:"manifest"`
	Files    Files    `json:"files"`
}

// Kind reads one kind of package. Describing is pure: it reads the files, checks them and spells
// out the permissions, and touches nothing.
type Kind interface {
	Kind() string
	Describe(it Item, files Files) (Manifest, error)
}

// Kinds are the kinds a server knows, by name.
type Kinds map[string]Kind

// KindsOf builds the set.
func KindsOf(ks ...Kind) Kinds {
	out := Kinds{}
	for _, k := range ks {
		out[k.Kind()] = k
	}
	return out
}

// Names lists the kinds, sorted.
func (ks Kinds) Names() []string {
	out := make([]string, 0, len(ks))
	for name := range ks {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Describe reads a package with its kind.
func (ks Kinds) Describe(it Item, files Files) (Manifest, error) {
	k, ok := ks[it.Kind]
	if !ok {
		return Manifest{}, invalid("kind %q is not one this server installs", it.Kind)
	}
	m, err := k.Describe(it, files)
	if err != nil {
		return m, fmt.Errorf("%s %s: %w", it.Key(), it.Version, err)
	}
	if m.Name != it.Name || m.Version != it.Version {
		return m, invalid("%s: the index says %s %s, the package says %s %s", it.Path, it.Name, it.Version, m.Name, m.Version)
	}
	m.Kind = it.Kind
	m.Permissions = normalPermissions(m.Permissions)
	if m.Readme == "" {
		if f, ok := files.Get("README.md"); ok {
			m.Readme = string(f.Data)
		}
	}
	return m, nil
}

// normalPermissions trims, drops empty and repeated lines and sorts them, so two versions'
// permissions compare as sets.
func normalPermissions(in []string) []string {
	out := []string{}
	for _, p := range in {
		if p = strings.Join(strings.Fields(p), " "); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// NewPermissions are the lines of asked that accepted does not hold: what a person has to accept
// before a package (or its new version) goes in.
func NewPermissions(asked, accepted []string) []string {
	have := map[string]bool{}
	for _, p := range normalPermissions(accepted) {
		have[p] = true
	}
	var out []string
	for _, p := range normalPermissions(asked) {
		if !have[p] {
			out = append(out, p)
		}
	}
	return out
}

// readYAML decodes a package's YAML manifest strictly: a field the server does not know is more
// likely a typo that would silently drop a limit than something to ignore.
func readYAML(files Files, name string, out any) error {
	f, ok := files.Get(name)
	if !ok {
		return invalid("%s is missing", name)
	}
	dec := yaml.NewDecoder(bytes.NewReader(f.Data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return invalid("%s: %v", name, err)
	}
	return nil
}

// detail is a kind's manifest as JSON, for the catalogue.
func detail(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// textFile is a file a manifest names, which must be there and be text.
func textFile(files Files, name string) (string, error) {
	if err := SafePath(name); err != nil {
		return "", err
	}
	f, ok := files.Get(name)
	if !ok {
		return "", invalid("%s is missing", name)
	}
	if bytes.IndexByte(f.Data, 0) >= 0 {
		return "", invalid("%s is not text", name)
	}
	return string(f.Data), nil
}

func requireText(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalid("%s is required", field)
	}
	return nil
}
