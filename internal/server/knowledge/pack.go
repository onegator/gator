// Package knowledge holds how this workspace builds software: packs fetched from a registry
// and entries written by hand, composed into the part of a job's prompt that is the same for
// every project. Product context, which never leaves its project, lives elsewhere.
package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Scopes a pack or an entry can cover.
var Scopes = []string{"language", "architecture", "requirements", "testing", "security"}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Manifest is a pack's pack.json.
type Manifest struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Scope     string   `json:"scope"`
	AppliesTo []string `json:"applies_to,omitempty"` // free tags a project matches, e.g. "go", "swift"
	Roles     []string `json:"roles,omitempty"`      // empty: every role
	Phases    []string `json:"phases,omitempty"`     // empty: every phase
	Files     []string `json:"files"`                // Markdown files, in reading order
}

// File is one Markdown document of a pack.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Pack is a manifest with its documents.
type Pack struct {
	Manifest Manifest `json:"manifest"`
	Files    []File   `json:"files"`
}

// Validate checks a manifest before anything is stored.
func (m Manifest) Validate() error {
	if !nameRe.MatchString(m.Name) {
		return fmt.Errorf("pack name %q must be lowercase letters, digits and dashes", m.Name)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("pack %s has no version", m.Name)
	}
	if !contains(Scopes, m.Scope) {
		return fmt.Errorf("pack %s: scope %q is not one of %s", m.Name, m.Scope, strings.Join(Scopes, ", "))
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("pack %s lists no files", m.Name)
	}
	for _, f := range m.Files {
		if strings.Contains(f, "..") || strings.HasPrefix(f, "/") {
			return fmt.Errorf("pack %s: file %q must stay inside the pack", m.Name, f)
		}
		if !strings.HasSuffix(f, ".md") {
			return fmt.Errorf("pack %s: %q is not Markdown", m.Name, f)
		}
	}
	return nil
}

// Applies reports whether a pack belongs in a prompt for this role and phase, given the
// project's tags. A pack that names neither roles nor phases applies to all of them.
func (m Manifest) Applies(role, phase string, tags []string) bool {
	if len(m.Roles) > 0 && !contains(m.Roles, role) {
		return false
	}
	if len(m.Phases) > 0 && !contains(m.Phases, phase) {
		return false
	}
	if len(m.AppliesTo) > 0 {
		for _, t := range m.AppliesTo {
			if contains(tags, t) {
				return true
			}
		}
		return false
	}
	return true
}

// Checksum is the pack's fingerprint: its manifest and every file, in a fixed order. The
// registry index carries the same value, so a tampered or half-fetched pack is refused.
func (p Pack) Checksum() string {
	files := append([]File(nil), p.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	h := sha256.New()
	manifest, _ := json.Marshal(p.Manifest)
	h.Write(manifest)
	h.Write([]byte{0})
	for _, f := range files {
		h.Write([]byte(f.Path))
		h.Write([]byte{0})
		h.Write([]byte(f.Content))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Text is the pack as one document, its files in the manifest's order.
func (p Pack) Text() string {
	byPath := map[string]string{}
	for _, f := range p.Files {
		byPath[f.Path] = f.Content
	}
	var b strings.Builder
	for _, name := range p.Manifest.Files {
		if body, ok := byPath[name]; ok {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(strings.TrimSpace(body))
		}
	}
	return b.String()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
