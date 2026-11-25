package marketplace

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/onegator/gator/internal/server/process"
)

// ProcessFile is a process package's manifest.
const ProcessFile = "process.yaml"

// ProcessManifest is process.yaml: the process of one kind of task (a template like those in
// internal/server/process/defaults) and the prompts of the roles it brings.
type ProcessManifest struct {
	Name        string            `yaml:"name" json:"name"`
	Version     string            `yaml:"version" json:"version"`
	DisplayName string            `yaml:"display_name" json:"display_name"`
	Description string            `yaml:"description" json:"description"`
	Phases      []string          `yaml:"phases,omitempty" json:"phases,omitempty"`
	Icon        string            `yaml:"icon,omitempty" json:"icon,omitempty"`
	TaskKind    string            `yaml:"kind" json:"kind"`         // the kind of task it is the process of
	Template    string            `yaml:"template" json:"template"` // the template's file
	Roles       map[string]string `yaml:"roles,omitempty" json:"roles,omitempty"`
	Permissions []string          `yaml:"permissions,omitempty" json:"permissions,omitempty"`
}

// ProcessPackage is a process package read whole: its template and its roles' prompts.
type ProcessPackage struct {
	Manifest ProcessManifest
	Template process.Template
	Roles    map[string]string
}

// Process is the process kind.
type Process struct{}

// Kind implements Kind.
func (Process) Kind() string { return KindProcess }

// Describe implements Kind.
func (Process) Describe(_ Item, files Files) (Manifest, error) {
	pp, err := ParseProcess(files)
	if err != nil {
		return Manifest{}, err
	}
	pm := pp.Manifest
	m := Manifest{Name: pm.Name, Version: pm.Version, DisplayName: pm.DisplayName, Description: pm.Description,
		Phases: pm.Phases, Icon: pm.Icon, Detail: detail(pm)}
	m.Permissions = append(m.Permissions, pm.Permissions...)
	m.Permissions = append(m.Permissions, "sets the process of "+pm.TaskKind+" tasks: "+phaseNames(pp.Template))
	roles := make([]string, 0, len(pp.Roles))
	for r := range pp.Roles {
		roles = append(roles, r)
	}
	slices.Sort(roles)
	for _, r := range roles {
		m.Permissions = append(m.Permissions, "sets the prompt of the role "+r)
	}
	for _, p := range pp.Template.Phases {
		if p.Requires != "" {
			m.Requires.Capabilities = append(m.Requires.Capabilities, p.Requires)
		}
	}
	return m, nil
}

// ParseProcess reads and checks a process package.
func ParseProcess(files Files) (ProcessPackage, error) {
	var pp ProcessPackage
	pm := &pp.Manifest
	if err := readYAML(files, ProcessFile, pm); err != nil {
		return pp, err
	}
	for field, v := range map[string]string{"name": pm.Name, "version": pm.Version, "display_name": pm.DisplayName,
		"description": pm.Description, "kind": pm.TaskKind, "template": pm.Template} {
		if err := requireText(field, v); err != nil {
			return pp, err
		}
	}
	if !ValidName(pm.TaskKind) {
		return pp, invalid("kind %q must be lowercase letters, digits and dashes", pm.TaskKind)
	}
	raw, err := textFile(files, pm.Template)
	if err != nil {
		return pp, err
	}
	if err := yaml.Unmarshal([]byte(raw), &pp.Template); err != nil {
		return pp, invalid("%s: %v", pm.Template, err)
	}
	if err := pp.Template.Validate(); err != nil {
		return pp, invalid("%s: %v", pm.Template, err)
	}
	if pp.Template.Kind != pm.TaskKind {
		return pp, invalid("%s is the process of %s tasks, the manifest says %s", pm.Template, pp.Template.Kind, pm.TaskKind)
	}
	if pp.Template.Hidden {
		return pp, invalid("a hidden kind is Gator's own, not a package's")
	}
	pp.Roles = map[string]string{}
	for role, file := range pm.Roles {
		if !ValidName(strings.ReplaceAll(role, "_", "-")) {
			return pp, invalid("role %q is not a name", role)
		}
		prompt, err := textFile(files, file)
		if err != nil {
			return pp, fmt.Errorf("role %s: %w", role, err)
		}
		if strings.TrimSpace(prompt) == "" {
			return pp, invalid("role %s: %s is empty", role, file)
		}
		pp.Roles[role] = prompt
	}
	return pp, nil
}

func phaseNames(t process.Template) string {
	names := make([]string, 0, len(t.Phases))
	for _, p := range t.Phases {
		names = append(names, p.Name)
	}
	return strings.Join(names, " → ")
}
