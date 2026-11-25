package marketplace

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/onegator/gator/internal/server/netguard"
)

// Snapshot is a registry's files as one fetch found them. Close removes whatever the fetch left
// on disk.
type Snapshot struct {
	FS    fs.FS
	Close func()
}

// Fetcher brings a registry's files. It checks nothing: what it brings is read only through a
// Verifier.
type Fetcher interface {
	Fetch(ctx context.Context, url string) (Snapshot, error)
}

// Git shallow-clones a registry repository, over https or ssh only, from a host the outbound
// policy lets the server reach (checked before git connects: git dials on its own).
type Git struct {
	Timeout time.Duration // default 2 minutes
	Net     netguard.Policy
}

// remoteHost is the host a git address names, and whether git may fetch from it at all.
func remoteHost(raw string, p netguard.Policy) (string, error) {
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		switch u.Scheme {
		case "https", "ssh":
			return u.Hostname(), nil
		case "http":
			if err := p.CheckScheme(u); err != nil {
				return "", fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			return u.Hostname(), nil
		}
		return "", invalid("a registry is fetched over https or ssh, not %s", u.Scheme)
	}
	// scp-like ssh: user@host:path
	if at := strings.Index(raw, "@"); at >= 0 {
		if colon := strings.Index(raw[at:], ":"); colon > 0 {
			return raw[at+1 : at+colon], nil
		}
	}
	return "", invalid("%q is not a registry's address", raw)
}

// Fetch implements Fetcher.
func (g Git) Fetch(ctx context.Context, u string) (Snapshot, error) {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	host, err := remoteHost(u, g.Net)
	if err != nil {
		return Snapshot{}, err
	}
	if err := g.Net.CheckHost(ctx, host); err != nil {
		return Snapshot{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	dir, err := os.MkdirTemp("", "gator-marketplace-")
	if err != nil {
		return Snapshot{}, err
	}
	done := func() { _ = os.RemoveAll(dir) }
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// No hooks, no prompts, no submodules: a registry is read, never run.
	// Only the transports the address was checked for, and no redirect to another host.
	cmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always", "-c", "protocol.http.allow="+allowHTTP(g.Net),
		"-c", "http.followRedirects=false", "clone", "--depth", "1", "--quiet", "--no-recurse-submodules", "--", u, dir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	if out, err := cmd.CombinedOutput(); err != nil {
		done()
		return Snapshot{}, fmt.Errorf("cloning the registry: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return rooted(dir, done)
}

// rooted is a snapshot of dir read through os.Root: no path, and no link, leads out of it.
func rooted(dir string, done func()) (Snapshot, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		done()
		return Snapshot{}, err
	}
	return Snapshot{FS: root.FS(), Close: func() { _ = root.Close(); done() }}, nil
}

func allowHTTP(p netguard.Policy) string {
	if p.AllowPrivate {
		return "always"
	}
	return "never"
}

// Dir reads a registry from a directory on the server: file:// URLs and absolute paths, for a
// registry checked out by hand, an air-gapped install, or a test.
type Dir struct{}

// Fetch implements Fetcher.
func (Dir) Fetch(_ context.Context, u string) (Snapshot, error) {
	p := u
	if strings.HasPrefix(u, "file://") {
		parsed, err := url.Parse(u)
		if err != nil {
			return Snapshot{}, invalid("%q is not a file URL", u)
		}
		p = parsed.Path
	}
	if !filepath.IsAbs(p) {
		return Snapshot{}, invalid("a registry directory must be an absolute path")
	}
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		return Snapshot{}, invalid("%s is not a directory", p)
	}
	return rooted(p, func() {})
}

// ByScheme picks a fetcher by the registry's address: a directory for file:// and absolute paths,
// git for anything else.
type ByScheme struct {
	Local  Fetcher
	Remote Fetcher
}

// FetcherFor reads directories and clones everything else, within the outbound policy.
func FetcherFor(p netguard.Policy) Fetcher { return ByScheme{Local: Dir{}, Remote: Git{Net: p}} }

// Fetch implements Fetcher.
func (b ByScheme) Fetch(ctx context.Context, u string) (Snapshot, error) {
	if strings.TrimSpace(u) == "" {
		return Snapshot{}, invalid("the registry has no address")
	}
	if Local(u) {
		return b.Local.Fetch(ctx, u)
	}
	return b.Remote.Fetch(ctx, u)
}

// Local says whether u names a directory on the server rather than a repository.
func Local(u string) bool { return strings.HasPrefix(u, "file://") || filepath.IsAbs(u) }
