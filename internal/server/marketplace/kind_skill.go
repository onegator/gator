package marketplace

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillFile is the file every skill folder has, in the Agent Skills format: YAML frontmatter
// (name, description, …) and the instructions an agent reads when the skill applies.
const SkillFile = "SKILL.md"

// SkillManifest is a SKILL.md's frontmatter.
type SkillManifest struct {
	Name          string            `yaml:"name" json:"name"`
	Description   string            `yaml:"description" json:"description"`
	Version       string            `yaml:"version,omitempty" json:"version,omitempty"`
	ExamplePrompt string            `yaml:"example-prompt,omitempty" json:"example_prompt,omitempty"`
	License       string            `yaml:"license,omitempty" json:"license,omitempty"`
	AllowedTools  yaml.Node         `yaml:"allowed-tools,omitempty" json:"-"`
	Compatibility string            `yaml:"compatibility,omitempty" json:"compatibility,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty" json:"metadata,omitempty"`
	// Tools is allowed-tools read as a list, whichever way it was written.
	Tools []string `yaml:"-" json:"allowed_tools,omitempty"`
}

// maxSkillDescription is what the Agent Skills format allows a description.
const maxSkillDescription = 1024

// ParseSkill reads a SKILL.md: its frontmatter and its body.
func ParseSkill(data []byte) (SkillManifest, string, error) {
	var m SkillManifest
	text := string(bytes.TrimPrefix(data, []byte("\ufeff")))
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return m, "", invalid("%s must start with a --- frontmatter", SkillFile)
	}
	front, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return m, "", invalid("%s's frontmatter has no closing ---", SkillFile)
	}
	dec := yaml.NewDecoder(strings.NewReader(front))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return m, "", invalid("%s frontmatter: %v", SkillFile, err)
	}
	switch m.AllowedTools.Kind {
	case yaml.ScalarNode:
		m.Tools = strings.Fields(m.AllowedTools.Value)
	case yaml.SequenceNode:
		if err := m.AllowedTools.Decode(&m.Tools); err != nil {
			return m, "", invalid("%s allowed-tools: %v", SkillFile, err)
		}
	}
	if !ValidName(m.Name) {
		return m, "", invalid("skill name %q must be lowercase letters, digits and dashes", m.Name)
	}
	if err := requireText("description", m.Description); err != nil {
		return m, "", err
	}
	if len(m.Description) > maxSkillDescription {
		return m, "", invalid("a skill's description is at most %d characters", maxSkillDescription)
	}
	_, body, _ = strings.Cut(body, "\n") // the rest of the closing line
	return m, strings.TrimSpace(body), nil
}

// Skill is the skill kind: one folder in the Agent Skills format.
type Skill struct{}

// Kind implements Kind.
func (Skill) Kind() string { return KindSkill }

// Describe implements Kind.
func (Skill) Describe(it Item, files Files) (Manifest, error) {
	m, err := describeSkill(files)
	if err != nil {
		return m, err
	}
	if m.Version == "" {
		m.Version = it.Version
	}
	return m, nil
}

// describeSkill reads a skill's folder. Its permissions say what it brings an agent beyond
// words: scripts the agent may run (in the job, with the job's own rights — never at install)
// and tools it asks for.
func describeSkill(files Files) (Manifest, error) {
	f, ok := files.Get(SkillFile)
	if !ok {
		return Manifest{}, invalid("%s is missing", SkillFile)
	}
	sm, body, err := ParseSkill(f.Data)
	if err != nil {
		return Manifest{}, err
	}
	if body == "" {
		return Manifest{}, invalid("%s has no instructions", SkillFile)
	}
	if sm.Version == "" {
		sm.Version = sm.Metadata["version"] // where the Agent Skills format keeps what it does not name
	}
	m := Manifest{Name: sm.Name, Version: sm.Version, DisplayName: sm.Name, Description: sm.Description,
		ExamplePrompt: sm.ExamplePrompt, Readme: body, Detail: detail(sm)}
	if icon, ok := files.Get("assets/icon.png"); ok {
		m.Icon = icon.Path
	}
	if runnable := runnables(files); len(runnable) > 0 {
		m.Permissions = append(m.Permissions, fmt.Sprintf("skill %s ships files an agent may run in its job: %s", sm.Name, strings.Join(runnable, ", ")))
	}
	if len(sm.Tools) > 0 {
		m.Permissions = append(m.Permissions, fmt.Sprintf("skill %s asks for tools: %s", sm.Name, strings.Join(sm.Tools, ", ")))
	}
	return m, nil
}

// documents are what a skill carries that no agent runs: its words and its pictures.
var documents = []string{".md", ".txt", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp"}

// runnables are the files of a skill an agent could run: anything not a document, wherever it
// lies, and any file that starts like a script.
func runnables(files Files) []string {
	var out []string
	for _, f := range files {
		ext := strings.ToLower(path.Ext(f.Path))
		if !slices.Contains(documents, ext) || bytes.HasPrefix(f.Data, []byte("#!")) {
			out = append(out, f.Path)
		}
	}
	slices.Sort(out)
	return out
}
