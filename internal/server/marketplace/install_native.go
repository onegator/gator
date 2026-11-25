package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// Downloader brings a binary from where its manifest says it is.
type Downloader interface {
	Download(ctx context.Context, url string) (io.ReadCloser, error)
}

// HTTPDownloader downloads over HTTP(S), and reads file:// URLs from the server's disk (a mirror,
// an air-gapped install, a test).
type HTTPDownloader struct {
	HTTP *http.Client // default: a 5 minute timeout
}

// Download implements Downloader.
func (d HTTPDownloader) Download(ctx context.Context, raw string) (io.ReadCloser, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, invalid("%q is not an address", raw)
	}
	if u.Scheme == "file" {
		return os.Open(u.Path)
	}
	client := d.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
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

// pluginHost installs a plugin from a command on the server. *plugins.Host is one.
type pluginHost interface {
	Install(ctx context.Context, name string, command []string) (plugin.Manifest, error)
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
}

// Track implements Track.
func (NativeTrack) Track() string { return IntegrationNative }

// Scopes implements Track.
func (NativeTrack) Scopes() Scopes { return Scopes{Workspace: true} }

// Install implements Track.
func (n NativeTrack) Install(ctx context.Context, tx *db.Queries, in Input, im IntegrationManifest) (Installed, error) {
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
	path, err := n.fetch(ctx, im, b)
	if err != nil {
		return Installed{}, err
	}
	if _, err := n.Host.Install(ctx, im.Name, []string{path}); err != nil {
		_ = os.Remove(path)
		return Installed{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	p, err := tx.GetPluginByName(ctx, im.Name)
	if err != nil {
		return Installed{}, err
	}
	return Installed{Target: p.ID, State: map[string]string{"binary": path}}, nil
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
	made, err := n.Install(ctx, tx, in, im)
	if err != nil {
		return Installed{}, err
	}
	if old.Version != im.Version && n.Dir != "" && ValidName(old.Name) && ValidVersion(old.Version) {
		_ = os.RemoveAll(filepath.Join(n.Dir, old.Name, old.Version))
	}
	return made, nil
}
