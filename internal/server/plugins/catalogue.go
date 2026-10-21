package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/version"
	"github.com/onegator/gator/plugin"
)

// DefaultCatalogueDir is where the server looks for the plugins it ships: GATOR_PLUGIN_DIR, or
// plugins/ next to the server's own binary, which is where `make build plugins` puts both.
func DefaultCatalogueDir() string {
	if d := os.Getenv("GATOR_PLUGIN_DIR"); d != "" {
		return d
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "plugins")
}

// Bundled is a plugin the server ships. Its command is trusted the way the server's own binary
// is, which is what lets a project admin switch it on without a workspace admin installing it.
type Bundled struct {
	Name     string
	Command  []string
	Manifest plugin.Manifest
}

type probed struct {
	mod  time.Time
	size int64
	m    plugin.Manifest
	err  error
}

// Catalogue lists the bundled plugins. Each binary is started once to read its manifest, and
// again only when the file changes, so listing is cheap enough for every settings screen.
func (h *Host) Catalogue(ctx context.Context) []Bundled {
	dir := h.cfg.CatalogueDir
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			h.log.Warn("plugin catalogue", "dir", dir, "err", err)
		}
		return nil
	}
	var out []Bundled
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 || !plugin.ValidName(e.Name()) {
			continue
		}
		m, err := h.probeCached(ctx, path, info)
		if err != nil {
			h.log.Warn("bundled plugin does not start", "path", path, "err", err)
			continue
		}
		if m.Name != e.Name() {
			h.log.Warn("bundled plugin names itself differently", "path", path, "name", m.Name)
			continue
		}
		out = append(out, Bundled{Name: m.Name, Command: []string{path}, Manifest: m})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func (h *Host) probeCached(ctx context.Context, path string, info os.FileInfo) (plugin.Manifest, error) {
	h.catMu.Lock()
	c, ok := h.cat[path]
	h.catMu.Unlock()
	if ok && c.mod.Equal(info.ModTime()) && c.size == info.Size() {
		return c.m, c.err
	}
	m, err := h.probe(ctx, []string{path})
	if err == nil {
		err = m.Validate()
	}
	if err == nil && !plugin.CoreSatisfies(version.Version, m.MinCoreVersion) {
		err = fmt.Errorf("needs gator %s or newer", m.MinCoreVersion)
	}
	if ctx.Err() == nil {
		h.catMu.Lock()
		h.cat[path] = probed{mod: info.ModTime(), size: info.Size(), m: m, err: err}
		h.catMu.Unlock()
	}
	return m, err
}

// bundled finds one plugin of the catalogue.
func (h *Host) bundled(ctx context.Context, name string) (Bundled, bool) {
	for _, b := range h.Catalogue(ctx) {
		if b.Name == name {
			return b, true
		}
	}
	return Bundled{}, false
}

// ensureInstalled installs a bundled plugin the first time a project asks for it. A plugin a
// workspace admin installed from a command of their own is left as it is.
func (h *Host) ensureInstalled(ctx context.Context, name string) (db.Plugin, error) {
	q := db.New(h.pool)
	p, err := q.GetPluginByName(ctx, name)
	if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	b, ok := h.bundled(ctx, name)
	if !ok {
		return p, err
	}
	if _, err := h.Install(ctx, name, b.Command); err != nil {
		return p, err
	}
	return q.GetPluginByName(ctx, name)
}

// refreshBundled brings the stored manifest of every installed bundled plugin up to date with
// the binary now on disk. An upgrade replaces the binaries; without this, a project would keep
// the manifest of the old one — its hooks, its settings — until somebody reinstalled it by hand.
func (h *Host) refreshBundled(ctx context.Context) {
	cat := h.Catalogue(ctx)
	if len(cat) == 0 {
		return
	}
	installed, err := db.New(h.pool).ListPlugins(ctx)
	if err != nil {
		return
	}
	for _, p := range installed {
		for _, b := range cat {
			if b.Name != p.Name || len(p.Command) != 1 || p.Command[0] != b.Command[0] {
				continue
			}
			mb, _ := json.Marshal(b.Manifest)
			if bytes.Equal(normalJSON(p.Manifest), normalJSON(mb)) {
				continue
			}
			if _, err := h.Install(ctx, b.Name, b.Command); err != nil {
				h.log.Warn("refreshing a bundled plugin", "plugin", b.Name, "err", err)
			} else {
				h.log.Info("bundled plugin updated", "plugin", b.Name, "version", b.Manifest.Version)
			}
		}
	}
}

// normalJSON re-encodes JSON so jsonb's reordering does not read as a change.
func normalJSON(b []byte) []byte {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	out, _ := json.Marshal(v)
	return out
}

// CatalogueEntry is a bundled plugin as a settings screen offers it.
type CatalogueEntry struct {
	Bundled
	Installed bool
	Connect   *ConnectView // Connected is never set: the catalogue belongs to no project
}

// CatalogueEntries lists what a project can switch on with a click.
func (h *Host) CatalogueEntries(ctx context.Context) ([]CatalogueEntry, error) {
	installed, err := db.New(h.pool).ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, p := range installed {
		have[p.Name] = true
	}
	var out []CatalogueEntry
	for _, b := range h.Catalogue(ctx) {
		out = append(out, CatalogueEntry{Bundled: b, Installed: have[b.Name], Connect: h.connectView(b.Name, b.Manifest, nil)})
	}
	return out, nil
}
