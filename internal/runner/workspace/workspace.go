// Package workspace gives every job its own git worktree. Repositories are cached once per
// URL as bare clones under Root/repos; each job gets Root/jobs/<job id> on its own branch.
package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
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
	Branch string // empty for a read-only checkout, which is on no branch
	Base   string // commit the branch started from; empty without a repo
	// ReadOnly is a checkout nobody may write in: its files, its directories and its git metadata
	// are not writable. Read is the branch it reads.
	ReadOnly bool
	Read     string
	meta     string // a read-only worktree's git metadata in the cache
	bare     string
	hooks    string // the job's git hooks, outside the checkout; see SignCommits
	m        *Manager
	// job is the job's id; gitDir the worktree's git directory as Prepare made it, which a
	// checkpoint names rather than reads from the worktree's .git file; checkpoints are the
	// commits of the checkpoints so far.
	job         string
	gitDir      string
	checkpoints []string
	// format is the repository's object format (sha1, sha256); hashed what each file was when the
	// runner last hashed it, so the next checkpoint hashes only what changed.
	format string
	hashed map[string]staged
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
	dir, err := m.jobDir(job)
	if err != nil {
		return nil, err
	}
	if job.Repo == nil || job.Repo.URL == "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		return &Workspace{Dir: dir, m: m}, nil
	}
	bare, def, unlock, err := m.cache(ctx, job)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if out, _ := m.run(ctx, bare, "config", "--get", "user.email"); strings.TrimSpace(out) == "" {
		_, _ = m.run(ctx, bare, "config", "user.name", "Gator Runner")
		_, _ = m.run(ctx, bare, "config", "user.email", "runner@gator.local")
	}
	br := fmt.Sprintf("gator/%s-%s", short(job.TaskID), short(job.JobID))
	_ = setWritable(filepath.Join(bare, "worktrees", filepath.Base(dir)), true) // a read-only attempt before
	_, _ = m.run(ctx, bare, "worktree", "prune")
	_, _ = m.run(ctx, bare, "branch", "-D", br) // leftover from an earlier attempt
	if _, err := m.run(ctx, bare, "worktree", "add", "--quiet", "-b", br, dir, "refs/remotes/origin/"+def); err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	// An agent commits here. A machine that signs commits by default would make it wait on a
	// signing agent that no runner has, so this worktree never signs.
	if _, err := m.run(ctx, dir, "config", "commit.gpgsign", "false"); err != nil {
		return nil, fmt.Errorf("worktree config: %w", err)
	}
	base, err := m.run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	gitDir, err := m.run(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	w := &Workspace{Dir: dir, Branch: br, Base: strings.TrimSpace(base), bare: bare, m: m, job: job.JobID, gitDir: strings.TrimSpace(gitDir)}
	w.dropCheckpoints(ctx) // an earlier attempt's
	return w, nil
}

// PrepareReadOnly creates a job directory its agent cannot change: a checkout of origin/<default
// branch> on no branch (there is nothing to commit to, and nothing to push), with every file and
// directory in it made unwritable. It is the workspace of a chat-tier job, which reads and
// answers; the agent's tools do not write either, but tools are not a sandbox.
func (m *Manager) PrepareReadOnly(ctx context.Context, job proto.Job) (*Workspace, error) {
	dir, err := m.jobDir(job)
	if err != nil {
		return nil, err
	}
	ws := &Workspace{Dir: dir, ReadOnly: true, m: m}
	if job.Repo == nil || job.Repo.URL == "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	} else {
		bare, def, unlock, err := m.cache(ctx, job)
		if err != nil {
			return nil, err
		}
		ref := "refs/remotes/origin/" + def
		if b := job.Repo.ReadBranch; b != "" && !strings.HasPrefix(b, "-") {
			if _, err := m.run(ctx, bare, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+b); err == nil {
				ref = "refs/remotes/origin/" + b
			}
		}
		_ = setWritable(filepath.Join(bare, "worktrees", filepath.Base(dir)), true) // a read-only attempt before
		_, _ = m.run(ctx, bare, "worktree", "prune")
		_, err = m.run(ctx, bare, "worktree", "add", "--quiet", "--detach", dir, ref)
		unlock()
		if err != nil {
			return nil, fmt.Errorf("worktree: %w", err)
		}
		base, err := m.run(ctx, dir, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		ws.Base, ws.bare, ws.Read = strings.TrimSpace(base), bare, strings.TrimPrefix(ref, "refs/remotes/origin/")
		// The worktree's own git metadata lives in the cache; it is made unwritable too, so
		// not even git can record a commit here.
		if gitDir, err := m.run(ctx, dir, "rev-parse", "--absolute-git-dir"); err == nil {
			ws.meta = strings.TrimSpace(gitDir)
		}
	}
	for _, d := range []string{dir, ws.meta} {
		if err := setWritable(d, false); err != nil {
			return nil, fmt.Errorf("making the checkout read-only: %w", err)
		}
	}
	return ws, nil
}

// jobDir is the job's directory, emptied of a previous attempt of the same job.
func (m *Manager) jobDir(job proto.Job) (string, error) {
	dir := filepath.Join(m.Root, "jobs", job.JobID)
	_ = setWritable(dir, true) // a read-only attempt before this one
	return dir, os.RemoveAll(dir)
}

// cache refreshes the bare clone of the job's repository and answers it with the default branch,
// holding the repository's lock until unlock is called.
func (m *Manager) cache(ctx context.Context, job proto.Job) (bare, def string, unlock func(), err error) {
	if strings.HasPrefix(job.Repo.URL, "-") {
		return "", "", nil, fmt.Errorf("refusing repository url %q", job.Repo.URL)
	}
	def = job.Repo.DefaultBranch
	if def == "" {
		def = "main"
	}
	sum := sha256.Sum256([]byte(job.Repo.URL))
	bare = filepath.Join(m.Root, "repos", hex.EncodeToString(sum[:8])+".git")

	lock := m.repoLock(bare)
	lock.Lock()
	unlock = lock.Unlock
	if err := m.fetch(ctx, job.Repo.URL, bare); err != nil {
		unlock()
		return "", "", nil, err
	}
	return bare, def, unlock, nil
}

// fetch clones the repository bare the first time and fetches it every time.
func (m *Manager) fetch(ctx context.Context, url, bare string) error {

	if _, err := os.Stat(bare); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
			return err
		}
		if _, err := m.run(ctx, "", "clone", "--bare", "--quiet", "--", url, bare); err != nil {
			return fmt.Errorf("clone: %w", err)
		}
		// A bare clone has no fetch refspec; track the remote under refs/remotes/origin so the
		// job branches never collide with it. (Not --mirror: pushing to a mirror remote
		// behaves like push --mirror and could delete remote branches.)
		if _, err := m.run(ctx, bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return err
		}
	}
	if _, err := m.run(ctx, bare, "fetch", "--quiet", "--prune", "origin"); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	return nil
}

// setWritable makes everything under dir writable by its owner again, or unwritable by anyone.
// A missing dir is nothing to do.
func setWritable(dir string, writable bool) error {
	if dir == "" {
		return nil
	}
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // a link's mode is its target's business
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if writable {
			mode |= 0o200
		} else {
			mode &^= 0o222
		}
		return os.Chmod(path, mode)
	})
}

// maxChangedPaths bounds what one job reports. A change that touches more files than this says
// something about the whole repository, not about one part of it.
const maxChangedPaths = 500

// Changes returns the commits made on the branch, the files they changed, and how many files
// are modified but not committed.
func (w *Workspace) Changes(ctx context.Context) (commits []string, paths []string, dirty int, err error) {
	if !w.HasRepo() {
		return nil, nil, 0, nil
	}
	out, err := w.m.run(ctx, w.Dir, "log", "--format=%H", w.Base+"..HEAD")
	if err != nil {
		return nil, nil, 0, err
	}
	commits = lines(out)
	out, err = w.m.run(ctx, w.Dir, "diff", "--name-only", w.Base, "HEAD")
	if err != nil {
		return nil, nil, 0, err
	}
	paths = lines(out)
	if len(paths) > maxChangedPaths {
		paths = paths[:maxChangedPaths]
	}
	out, err = w.m.run(ctx, w.Dir, "status", "--porcelain")
	if err != nil {
		return nil, nil, 0, err
	}
	dirty = len(lines(out))
	return commits, paths, dirty, nil
}

// Push publishes the job branch to origin. Only the job branch is pushed; a read-only checkout
// has none.
func (w *Workspace) Push(ctx context.Context) error {
	if !w.HasRepo() || w.Branch == "" {
		return nil
	}
	_, err := w.m.run(ctx, w.Dir, "push", "--quiet", "origin", "HEAD:refs/heads/"+w.Branch)
	return err
}

// Cleanup removes the job directory. The local branch is deleted only if it was pushed
// (keep=false); an unpushed branch stays in the cache for recovery.
func (w *Workspace) Cleanup(ctx context.Context, keepBranch bool) error {
	if w.hooks != "" {
		_ = os.RemoveAll(w.hooks)
	}
	if w.ReadOnly {
		for _, d := range []string{w.Dir, w.meta} {
			if d == "" {
				continue
			}
			if err := setWritable(d, true); err != nil {
				return err
			}
		}
	}
	if !w.HasRepo() {
		return os.RemoveAll(w.Dir)
	}
	lock := w.m.repoLock(w.bare)
	lock.Lock()
	defer lock.Unlock()
	w.dropCheckpoints(ctx)
	if _, err := w.m.run(ctx, w.bare, "worktree", "remove", "--force", w.Dir); err != nil {
		_ = os.RemoveAll(w.Dir)
		_, _ = w.m.run(ctx, w.bare, "worktree", "prune")
	}
	if !keepBranch && w.Branch != "" {
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
	for l := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
