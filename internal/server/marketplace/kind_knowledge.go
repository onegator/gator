package marketplace

import (
	"encoding/json"
	"strings"

	"github.com/onegator/gator/internal/server/knowledge"
)

// PackFile is a knowledge pack's manifest.
const PackFile = "pack.json"

// Knowledge is the knowledge kind: a pack of how this workspace builds software, as
// knowledge_packs has always held them.
type Knowledge struct{}

// Kind implements Kind.
func (Knowledge) Kind() string { return KindKnowledge }

// Describe implements Kind.
func (Knowledge) Describe(_ Item, files Files) (Manifest, error) {
	pack, err := ParsePack(files)
	if err != nil {
		return Manifest{}, err
	}
	km := pack.Manifest
	desc := "How to write " + strings.Join(append([]string{km.Scope}, km.AppliesTo...), ", ")
	return Manifest{Name: km.Name, Version: km.Version, DisplayName: km.Name, Description: desc, Phases: km.Phases,
		Permissions: []string{"adds to the prompts of " + rolesOf(km) + " the conventions it carries (" + km.Scope + ")"},
		Detail:      detail(km)}, nil
}

// ParsePack reads a knowledge pack the way knowledge_packs stores it.
func ParsePack(files Files) (knowledge.Pack, error) {
	var pack knowledge.Pack
	f, ok := files.Get(PackFile)
	if !ok {
		return pack, invalid("%s is missing", PackFile)
	}
	if err := json.Unmarshal(f.Data, &pack.Manifest); err != nil {
		return pack, invalid("%s: %v", PackFile, err)
	}
	if err := pack.Manifest.Validate(); err != nil {
		return pack, invalid("%v", err)
	}
	for _, name := range pack.Manifest.Files {
		body, err := textFile(files, name)
		if err != nil {
			return pack, err
		}
		pack.Files = append(pack.Files, knowledge.File{Path: name, Content: body})
	}
	return pack, nil
}

func rolesOf(m knowledge.Manifest) string {
	if len(m.Roles) == 0 {
		return "every role"
	}
	return strings.Join(m.Roles, ", ")
}
