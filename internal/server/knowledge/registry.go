package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Index is a registry's index.json: which packs it offers and what each should hash to.
type Index struct {
	Packs []IndexEntry `json:"packs"`
}

// IndexEntry names a pack's folder in the repository and its expected checksum.
type IndexEntry struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Path     string `json:"path"`
	Checksum string `json:"checksum"`
}

// FetchTimeout bounds a registry fetch.
const FetchTimeout = 2 * time.Minute

// Fetch shallow-clones a registry and returns the packs whose contents match the checksum
// the index promises. A pack that does not match is refused, not silently used.
func Fetch(ctx context.Context, url string) ([]Pack, error) {
	if strings.TrimSpace(url) == "" {
		return nil, fmt.Errorf("registry: no url")
	}
	dir, err := os.MkdirTemp("", "gator-registry")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--quiet", url, dir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("registry clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return Read(dir)
}

// Read loads a registry from a directory, checking every pack against the index.
func Read(dir string) ([]Pack, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, fmt.Errorf("registry index: %w", err)
	}
	var index Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("registry index: %w", err)
	}
	var packs []Pack
	for _, entry := range index.Packs {
		pack, err := readPack(dir, entry)
		if err != nil {
			return nil, err
		}
		packs = append(packs, pack)
	}
	return packs, nil
}

func readPack(dir string, entry IndexEntry) (Pack, error) {
	var pack Pack
	if strings.Contains(entry.Path, "..") || strings.HasPrefix(entry.Path, "/") {
		return pack, fmt.Errorf("pack %s: path %q must stay inside the registry", entry.Name, entry.Path)
	}
	base := filepath.Join(dir, entry.Path)
	raw, err := os.ReadFile(filepath.Join(base, "pack.json"))
	if err != nil {
		return pack, fmt.Errorf("pack %s: %w", entry.Name, err)
	}
	if err := json.Unmarshal(raw, &pack.Manifest); err != nil {
		return pack, fmt.Errorf("pack %s manifest: %w", entry.Name, err)
	}
	if err := pack.Manifest.Validate(); err != nil {
		return pack, err
	}
	if entry.Name != "" && (pack.Manifest.Name != entry.Name || pack.Manifest.Version != entry.Version) {
		return pack, fmt.Errorf("pack %s: the index says %s %s, the manifest says %s %s",
			entry.Path, entry.Name, entry.Version, pack.Manifest.Name, pack.Manifest.Version)
	}
	for _, name := range pack.Manifest.Files {
		body, err := os.ReadFile(filepath.Join(base, name))
		if err != nil {
			return pack, fmt.Errorf("pack %s: %w", entry.Name, err)
		}
		pack.Files = append(pack.Files, File{Path: name, Content: string(body)})
	}
	if sum := pack.Checksum(); entry.Checksum != "" && sum != entry.Checksum {
		return pack, fmt.Errorf("pack %s %s: the index promises %s but the files hash to %s",
			entry.Name, entry.Version, entry.Checksum, sum)
	}
	return pack, nil
}

// ReadDir loads one pack from a folder holding pack.json and its Markdown.
func ReadDir(dir string) (Pack, error) {
	return readPack(filepath.Dir(dir), IndexEntry{Name: "", Version: "", Path: filepath.Base(dir)})
}
