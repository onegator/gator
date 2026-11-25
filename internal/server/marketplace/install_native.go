package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/netguard"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// Downloader brings a binary from where its manifest says it is.
type Downloader interface {
	Download(ctx context.Context, url string) (io.ReadCloser, error)
}

// HTTPDownloader downloads over https (through the server's outbound policy), and reads file://
// URLs from the server's disk only where the operator allows it (a mirror, an air-gapped install):
// otherwise a package could name any file the server can read.
type HTTPDownloader struct {
	HTTP      *http.Client // default: a 5 minute timeout over Net's transport
	Net       netguard.Policy
	AllowFile bool // GATOR_MARKETPLACE_ALLOW_FILE=1
}

// Download implements Downloader.
func (d HTTPDownloader) Download(ctx context.Context, raw string) (io.ReadCloser, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, invalid("%q is not an address", raw)
	}
	if u.Scheme == "file" {
		if !d.AllowFile {
			return nil, invalid("file:// downloads are off on this server (GATOR_MARKETPLACE_ALLOW_FILE)")
		}
		return os.Open(u.Path)
	}
	client := d.HTTP
	if client == nil {
		client = d.Net.Client(5 * time.Minute)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("downloading %s: %s", raw, res.Status)
	}
	return res.Body, nil
}

// pluginHost installs and removes plugins. *plugins.Host is one.
type pluginHost interface {
	Install(ctx context.Context, name string, command []string) (plugin.Manifest, error)
	Uninstall(ctx context.Context, id pgtype.UUID) error
	// Ships says whether the server ships a plugin of that name.
	Ships(ctx context.Context, name string) bool
}

// NativeTrack installs a native plugin: the build for this server's platform, downloaded and
// checked against the sha256 its manifest promises before it is made executable, then installed
// as any plugin is (the server starts it once to read its manifest). It is the workspace's, as
// plugins are; each project switches it on.
type NativeTrack struct {
	// Dir is where builds go: Dir/<name>/<version>/<name>, next to the bundled plugins
	// (GATOR_PLUGIN_DIR/marketplace), where the catalogue does not look.
	Dir      string
	Platform string // GOOS/GOARCH; default this server's
	Download Downloader
	Host     pluginHost
	MaxBytes int64 // default 200 MiB
	// Plugins reads plugins outside a transaction, to undo a failed install. *db.Queries is one.
	Plugins interface {
		GetPluginByName(ctx context.Context, name string) (db.Plugin, error)
	}
}

// Track implements Track.
func (NativeTrack) Track() string { return IntegrationNative }

// Scopes implements Track.
func (NativeTrack) Scopes() Scopes { return Scopes{Workspace: true} }

// Install implements Track.
func (n NativeTrack) Install(ctx context.Context, tx *db.Queries, in Input, im IntegrationManifest) (Installed, error) {
	return n.install(ctx, tx, in, im, pgtype.UUID{})
}

// install puts the build in place and installs the plugin from it. own is the plugin this
// installation made (an update), the only one whose name it may take: a package named like a
// plugin the server ships, or one somebody installed, would otherwise take over the command — and
// with it every project's secrets for that plugin.
func (n NativeTrack) install(ctx context.Context, tx *db.Queries, in Input, im IntegrationManifest, own pgtype.UUID) (Installed, error) {
	if n.Host == nil || n.Dir == "" {
		return Installed{}, invalid("this server does not install native plugins (no plugin directory)")
	}
	if !plugin.ValidName(im.Name) {
		return Installed{}, invalid("%q cannot name a plugin", im.Name)
	}
	platform := n.Platform
	if platform == "" {
		platform = runtime.GOOS + "/" + runtime.GOARCH
	}
	b, ok := im.Binaries[platform]
	if !ok {
		return Installed{}, invalid("%s has no build for %s", im.Name, platform)
	}
	if n.Host.Ships(ctx, im.Name) {
		return Installed{}, fmt.Errorf("%w: this server ships a plugin named %s; a package does not replace it", ErrConflict, im.Name)
	}
	switch cur, err := tx.GetPluginByName(ctx, im.Name); {
	case err == nil && cur.ID != own:
		return Installed{}, fmt.Errorf("%w: a plugin named %s is installed already, not from this package", ErrConflict, im.Name)
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return Installed{}, err
	}
	path, err := n.fetch(ctx, im, b)
	if err != nil {
		return Installed{}, err
	}
	dir := filepath.Dir(path)
	if _, err := n.Host.Install(ctx, im.Name, []string{path}); err != nil {
		n.forget(ctx, im.Name, own, dir)
		return Installed{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// The plugin is installed outside this transaction (its process is started to read it); if the
	// installation is not recorded after all, what was made goes again.
	in.undo(func() { n.forget(context.WithoutCancel(ctx), im.Name, own, dir) })
	p, err := tx.GetPluginByName(ctx, im.Name)
	if err != nil {
		return Installed{}, err
	}
	return Installed{Target: p.ID, State: map[string]string{"binary": path}}, nil
}

// forget takes away what a failed install made: the build, and the plugin's row unless it was
// this installation's before.
func (n NativeTrack) forget(ctx context.Context, name string, own pgtype.UUID, dir string) {
	_ = os.RemoveAll(dir)
	if own.Valid {
		return
	}
	if p, err := n.lookup(ctx, name); err == nil {
		_ = n.Host.Uninstall(ctx, p)
	}
}

// lookup finds a plugin's id outside any transaction.
func (n NativeTrack) lookup(ctx context.Context, name string) (pgtype.UUID, error) {
	if n.Plugins == nil {
		return pgtype.UUID{}, pgx.ErrNoRows
	}
	p, err := n.Plugins.GetPluginByName(ctx, name)
	return p.ID, err
}

// owned says whether a plugin's command is a build this track put in place.
func (n NativeTrack) owned(p db.Plugin) bool {
	if n.Dir == "" || len(p.Command) != 1 {
		return false
	}
	rel, err := filepath.Rel(filepath.Join(n.Dir, p.Name), p.Command[0])
	return err == nil && !strings.HasPrefix(rel, "..")
}

// fetch downloads a build to a file only this user can read, hashing it as it comes, and makes
// it executable only once it matches. One that does not is removed.
func (n NativeTrack) fetch(ctx context.Context, im IntegrationManifest, b Binary) (string, error) {
	max := n.MaxBytes
	if max == 0 {
		max = 200 << 20
	}
	dir := filepath.Join(n.Dir, im.Name, im.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // a no-op once it has its name
	body, err := n.Download.Download(ctx, b.URL)
	if err != nil {
		tmp.Close()
		return "", err
	}
	defer body.Close()
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, max+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if size > max {
		return "", invalid("the build of %s is larger than %d bytes", im.Name, max)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != b.SHA256 {
		return "", fmt.Errorf("%w: the build of %s %s hashes to %s, its manifest promises %s", ErrChecksum, im.Name, im.Version, sum, b.SHA256)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, im.Name)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// Remove implements Track: a plugin no project keeps settings for goes, its builds with it. One a
// project still has is left: switching it off keeps the project's settings, and those are the
// project's to drop.
func (n NativeTrack) Remove(ctx context.Context, tx *db.Queries, inst db.Installation) error {
	if !inst.TargetID.Valid {
		return nil
	}
	used, err := tx.CountProjectPluginsOf(ctx, inst.TargetID)
	if err != nil {
		return err
	}
	if used > 0 {
		return fmt.Errorf("%w: %d projects have %s set up; remove it from them first", ErrConflict, used, inst.Name)
	}
	p, err := tx.GetPluginByID(ctx, inst.TargetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !n.owned(p) {
		return nil // not a build of ours any more: whoever installed it keeps it
	}
	if _, err := tx.DeletePlugin(ctx, inst.TargetID); err != nil {
		return err
	}
	if n.Dir != "" && ValidName(inst.Name) {
		_ = os.RemoveAll(filepath.Join(n.Dir, inst.Name))
	}
	return nil
}

// Update implements the native track's update: the new version's build, checked as the first was,
// then the plugin installed from it; the old version's build goes.
func (n NativeTrack) Update(ctx context.Context, tx *db.Queries, inst db.Installation, in Input, old, im IntegrationManifest) (Installed, error) {
	made, err := n.install(ctx, tx, in, im, inst.TargetID)
	if err != nil {
		return Installed{}, err
	}
	if old.Version != im.Version && n.Dir != "" && ValidName(old.Name) && ValidVersion(old.Version) {
		_ = os.RemoveAll(filepath.Join(n.Dir, old.Name, old.Version))
	}
	return made, nil
}
