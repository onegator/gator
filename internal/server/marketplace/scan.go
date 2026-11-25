package marketplace

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// Folders are where a registry keeps each kind: one folder per package below them.
var Folders = map[string]string{
	"agents":       KindAgent,
	"skills":       KindSkill,
	"integrations": KindIntegration,
	"process":      KindProcess,
	"knowledge":    KindKnowledge,
}

// Scan builds the index of a registry checked out at fsys: every package folder under the kinds'
// folders, read by its kind, with its checksum. It is what a maintainer runs after changing a
// package (gator-server marketplace index), and what the registry's CI compares with the index.
func Scan(fsys fs.FS, kinds Kinds, lim Limits) (Index, error) {
	idx := Index{Items: []Item{}}
	folders := make([]string, 0, len(Folders))
	for f := range Folders {
		folders = append(folders, f)
	}
	slices.Sort(folders)
	for _, folder := range folders {
		kind, ok := kinds[Folders[folder]]
		if !ok {
			continue
		}
		entries, err := fs.ReadDir(fsys, folder)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			dir := path.Join(folder, e.Name())
			files, err := ReadFiles(fsys, dir, lim)
			if err != nil {
				return idx, fmt.Errorf("%s: %w", dir, err)
			}
			m, err := kind.Describe(Item{Kind: kind.Kind(), Name: e.Name(), Path: dir}, files)
			if err != nil {
				return idx, fmt.Errorf("%s: %w", dir, err)
			}
			if m.Name != e.Name() {
				return idx, invalid("%s: the folder is named %s, the package %s", dir, e.Name(), m.Name)
			}
			if !ValidVersion(m.Version) {
				return idx, invalid("%s: %q is not a version", dir, m.Version)
			}
			idx.Items = append(idx.Items, Item{Kind: kind.Kind(), Name: m.Name, Version: m.Version, Path: dir, Checksum: files.Checksum()})
		}
	}
	return idx, nil
}

// MarshalIndex writes an index the way registries keep it: one item a line, so a change to one
// package is one line of the diff.
func MarshalIndex(idx Index) []byte {
	var b strings.Builder
	b.WriteString("{\"items\": [\n")
	for i, it := range idx.Items {
		line, _ := json.Marshal(it)
		b.WriteString("  ")
		b.Write(line)
		if i < len(idx.Items)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]}\n")
	return []byte(b.String())
}
