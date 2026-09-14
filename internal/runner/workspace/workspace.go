// Package workspace gives every job its own git worktree. Repositories are cached once per
// URL as bare clones under Root/repos; each job gets Root/jobs/<job id> on its own branch.
package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/onegator/gator/internal/proto"
)

// Manager owns the cache and job directories.
type Manager struct {
	Root string
	Git  string // git binary; default "git"

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Workspace is one job's checkout.
type Workspace struct {
	Dir    string
	Branch string
	Base   string // commit the branch started from; empty without a repo
	bare   string
	m      *Manager
}

// HasRepo reports whether the workspace is a git checkout.
func (w *Workspace) HasRepo() bool { return w.bare != "" }

func (m *Manager) git() string {
	if m.Git == "" {
		return "git"
	}
	return m.Git
}

func (m *Manager) repoLock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	if m.locks[key] == nil {
		m.locks[key] = &sync.Mutex{}
	}
	return m.locks[key]
}

// Prepare creates the job directory. With a repo it refreshes the bare cache and adds a
// worktree on branch gator/<task>-<job> from origin/<default branch>.
func (m *Manager) Prepare(ctx context.Context, job proto.Job) (*Workspace, error) {
	dir := filepath.Join(m.Root, "jobs", job.JobID)
	if err := os.RemoveAll(dir); err != nil { // a previous attempt of the same job
		return nil, err
	}
	if job.Repo == nil || job.Repo.URL == "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		return &Workspace{Dir: dir, m: m}, nil
	}
	if strings.HasPrefix(job.Repo.URL, "-") {
		return nil, fmt.Errorf("refusing repository url %q", job.Repo.URL)
	}
	def := job.Repo.DefaultBranch
	if def == "" {
		def = "main"
	}
	sum := sha256.Sum256([]byte(job.Repo.URL))
	bare := filepath.Join(m.Root, "repos", hex.EncodeToString(sum[:8])+".git")

	lock := m.repoLock(bare)
	lock.Lock()
	defer lock.Unlock()

	if _, err := os.Stat(bare); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
			return nil, err
		}
		if _, err := m.run(ctx, "", "clone", "--bare", "--quiet", "--", job.Repo.URL, bare); err != nil {
			return nil, fmt.Errorf("clone: %w", err)
		}
		// A bare clone has no fetch refspec; track the remote under refs/remotes/origin so the
		// job branches never collide with it. (Not --mirror: pushing to a mirror remote
		// behaves like push --mirror and could delete remote branches.)
		if _, err := m.run(ctx, bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return nil, err
		}
	}
	if _, err := m.run(ctx, bare, "fetch", "--quiet", "--prune", "origin"); err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	if out, _ := m.run(ctx, bare, "config", "--get", "user.email"); strings.TrimSpace(out) == "" {
		_, _ = m.run(ctx, bare, "config", "user.name", "Gator Runner")
		_, _ = m.run(ctx, bare, "config", "user.email", "runner@gator.local")
	}
	br := fmt.Sprintf("gator/%s-%s", short(job.TaskID), short(job.JobID))
	_, _ = m.run(ctx, bare, "worktree", "prune")
	_, _ = m.run(ctx, bare, "branch", "-D", br) // leftover from an earlier attempt
	if _, err := m.run(ctx, bare, "worktree", "add", "--quiet", "-b", br, dir, "refs/remotes/origin/"+def); err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	base, err := m.run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return &Workspace{Dir: dir, Branch: br, Base: strings.TrimSpace(base), bare: bare, m: m}, nil
}

// Changes returns the commits made on the branch and the number of files they changed,
// plus how many files are modified but not committed.
func (w *Workspace) Changes(ctx context.Context) (commits []string, changed, dirty int, err error) {
	if !w.HasRepo() {
		return nil, 0, 0, nil
	}
	out, err := w.m.run(ctx, w.Dir, "log", "--format=%H", w.Base+"..HEAD")
	if err != nil {
		return nil, 0, 0, err
	}
	commits = lines(out)
	out, err = w.m.run(ctx, w.Dir, "diff", "--name-only", w.Base, "HEAD")
	if err != nil {
		return nil, 0, 0, err
	}
	changed = len(lines(out))
	out, err = w.m.run(ctx, w.Dir, "status", "--porcelain")
	if err != nil {
		return nil, 0, 0, err
	}
	dirty = len(lines(out))
	return commits, changed, dirty, nil
}

// Push publishes the job branch to origin. Only the job branch is pushed.
func (w *Workspace) Push(ctx context.Context) error {
	if !w.HasRepo() {
		return nil
	}
	_, err := w.m.run(ctx, w.Dir, "push", "--quiet", "origin", "HEAD:refs/heads/"+w.Branch)
	return err
}

// Cleanup removes the job directory. The local branch is deleted only if it was pushed
// (keep=false); an unpushed branch stays in the cache for recovery.
func (w *Workspace) Cleanup(ctx context.Context, keepBranch bool) error {
	if !w.HasRepo() {
		return os.RemoveAll(w.Dir)
	}
	lock := w.m.repoLock(w.bare)
	lock.Lock()
	defer lock.Unlock()
	if _, err := w.m.run(ctx, w.bare, "worktree", "remove", "--force", w.Dir); err != nil {
		_ = os.RemoveAll(w.Dir)
		_, _ = w.m.run(ctx, w.bare, "worktree", "prune")
	}
	if !keepBranch {
		_, _ = w.m.run(ctx, w.bare, "branch", "-D", w.Branch)
	}
	return nil
}

func (m *Manager) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, m.git(), args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

func short(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// lines splits command output into non-empty lines. Porcelain status lines contain a space
// ("?? file"), and file names may too, so words are the wrong unit.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
