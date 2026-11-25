package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/onegator/gator/internal/proto"
)

// A checkpoint is the whole worktree after one turn, as a commit the runner makes on a hidden ref
// (refs/gator/checkpoints/<job>/<n>) without touching what the agent works with: its branch, its
// HEAD and its index stay as they are. The diff of a turn is between two checkpoints, so it shows
// what the turn did to the files, whatever the agent committed, amended or rebased meanwhile.
//
// What the runner runs here is git, on a repository whose configuration and attributes the agent
// could have written: nothing in them may run a program (filters, fsmonitor, hooks, external
// diffs, text conversions) or point git at files outside the job's worktree (core.worktree, the
// .git file). The checkpoint names the job's git directory and worktree itself, from Prepare.

// emptyTree is git's empty tree: attributes read from it are none.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// checkpointRef is where checkpoint n of a job lives in the repository cache.
func checkpointRef(job string, n int) string {
	return fmt.Sprintf("refs/gator/checkpoints/%s/%d", job, n)
}

// CheckpointResult is one checkpoint: the turn's changes and the job's so far. The first
// checkpoint of a job (Seq 0) is the worktree as the job found it and has no changes.
type CheckpointResult struct {
	proto.Checkpoint
	Took time.Duration
}

// Checkpoint records the worktree as it is now and answers what changed since the last
// checkpoint, and since the first. exclude are paths that never go into a checkpoint (the
// runner's own files, should one sit in the worktree). A read-only checkout, or one with no
// repository, has nothing to checkpoint and answers nil.
func (w *Workspace) Checkpoint(ctx context.Context, exclude []string) (*CheckpointResult, error) {
	if !w.HasRepo() || w.ReadOnly || w.gitDir == "" {
		return nil, nil
	}
	started := time.Now()
	g := w.checkpointGit()
	index := w.checkpointIndex()
	if len(w.checkpoints) == 0 {
		// The agent's own index, copied, has the stat data that spares hashing every file; it is
		// only read. Without it (a worktree with no index yet) git hashes everything once.
		if err := seedIndex(filepath.Join(w.gitDir, "index"), index); err != nil {
			return nil, err
		}
	}
	filters, err := g.filterNames(ctx)
	if err != nil {
		return nil, err
	}
	add := append([]string{"add", "--all", "--", "."}, excludeSpecs(w.Dir, exclude)...)
	if _, err := g.run(ctx, index, filters, add...); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	tree, err := g.run(ctx, index, filters, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	n := len(w.checkpoints)
	commitArgs := []string{"commit-tree", strings.TrimSpace(tree), "-m", fmt.Sprintf("gator checkpoint %d of job %s", n, w.job)}
	if n > 0 {
		commitArgs = append(commitArgs, "-p", w.checkpoints[n-1])
	}
	head, err := g.run(ctx, index, filters, commitArgs...)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	head = strings.TrimSpace(head)
	if _, err := g.run(ctx, index, filters, "update-ref", checkpointRef(w.job, n), head); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	w.checkpoints = append(w.checkpoints, head)
	out := &CheckpointResult{Checkpoint: proto.Checkpoint{Seq: n, HeadCommit: head}}
	if n > 0 {
		out.BaseCommit = w.checkpoints[n-1]
		if out.Files, out.Diff, out.Truncated, err = g.changes(ctx, out.BaseCommit, head); err != nil {
			return nil, err
		}
		if out.WholeFiles, out.Whole, out.WholeTruncated, err = g.changes(ctx, w.checkpoints[0], head); err != nil {
			return nil, err
		}
	}
	out.Took = time.Since(started)
	out.TookMS = out.Took.Milliseconds()
	return out, nil
}

// dropCheckpoints removes the job's checkpoint refs and its cached index: checkpoints are never
// pushed, and they go with the worktree (the server keeps the diffs).
func (w *Workspace) dropCheckpoints(ctx context.Context) {
	if w.bare == "" || w.job == "" {
		return
	}
	g := checkpointGit{bin: w.m.git(), gitDir: w.bare}
	refs, _ := g.run(ctx, "", nil, "for-each-ref", "--format=%(refname)", "refs/gator/checkpoints/"+w.job+"/")
	for _, ref := range lines(refs) {
		_, _ = g.run(ctx, "", nil, "update-ref", "-d", strings.TrimSpace(ref))
	}
	_ = os.Remove(w.checkpointIndex())
	w.checkpoints = nil
}

// checkpointIndex is the index the checkpoints of a job are built in, kept between checkpoints
// beside the repository cache so the next one hashes only what changed.
func (w *Workspace) checkpointIndex() string {
	return filepath.Join(w.bare, "gator-index", w.job)
}

func seedIndex(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	_ = os.Remove(to)
	src, err := os.Open(from)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// excludeSpecs are pathspecs that leave the paths under dir among exclude out of a checkpoint.
func excludeSpecs(dir string, exclude []string) []string {
	var out []string
	for _, p := range exclude {
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue // outside the worktree: never in a checkpoint anyway
		}
		out = append(out, ":(exclude,top,literal)"+filepath.ToSlash(rel))
	}
	return out
}

// checkpointGit runs git for checkpoints: on the job's own git directory and worktree, named
// rather than discovered, with whatever could run a program switched off.
type checkpointGit struct {
	bin      string
	gitDir   string
	workTree string
	hooks    string // an empty directory: no hook runs on update-ref
}

func (w *Workspace) checkpointGit() checkpointGit {
	return checkpointGit{bin: w.m.git(), gitDir: w.gitDir, workTree: w.Dir, hooks: filepath.Join(w.bare, "gator-nohooks")}
}

// filterNames are the filter drivers the repository's configuration defines: each is switched off
// by name, since a clean filter is a program git would run on every file it adds.
func (g checkpointGit) filterNames(ctx context.Context) ([]string, error) {
	out, err := g.run(ctx, "", nil, "config", "--name-only", "--get-regexp", `^filter\.`)
	if err != nil && strings.TrimSpace(out) == "" {
		return nil, nil // exit 1: there are none
	}
	seen := map[string]bool{}
	var names []string
	for _, key := range lines(out) {
		key = strings.TrimSpace(key)
		i, j := strings.Index(key, "."), strings.LastIndex(key, ".")
		if i < 0 || j <= i {
			continue
		}
		if name := key[i+1 : j]; !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

func (g checkpointGit) run(ctx context.Context, index string, filters []string, args ...string) (string, error) {
	var out bytes.Buffer
	err := g.stream(ctx, index, filters, &out, -1, args...)
	return out.String(), err
}

// stream runs git with its output into w, at most limit bytes of it (-1: all of it). It answers
// errTooLong when there was more.
func (g checkpointGit) stream(ctx context.Context, index string, filters []string, w io.Writer, limit int, args ...string) error {
	full := []string{"--git-dir=" + g.gitDir}
	if g.workTree != "" {
		full = append(full, "--work-tree="+g.workTree)
	}
	full = append(full, "-c", "core.fsmonitor=false", "-c", "core.attributesFile="+os.DevNull, "-c", "diff.external=",
		"-c", "core.hooksPath="+g.hooksDir(), "-c", "core.quotePath=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false")
	for _, f := range filters {
		full = append(full, "-c", "filter."+f+".clean=", "-c", "filter."+f+".smudge=", "-c", "filter."+f+".process=",
			"-c", "filter."+f+".required=false")
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, g.bin, full...)
	cmd.Dir = g.gitDir
	if g.workTree != "" {
		cmd.Dir = g.workTree // pathspecs are the worktree's
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1",
		"GIT_ATTR_SOURCE="+emptyTree, "GIT_OPTIONAL_LOCKS=0",
		"GIT_AUTHOR_NAME=Gator Runner", "GIT_AUTHOR_EMAIL=runner@gator.local",
		"GIT_COMMITTER_NAME=Gator Runner", "GIT_COMMITTER_EMAIL=runner@gator.local")
	if index != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var copyErr error
	if limit < 0 {
		_, copyErr = io.Copy(w, stdout)
	} else {
		n, err := io.Copy(w, io.LimitReader(stdout, int64(limit)+1))
		copyErr = err
		if n > int64(limit) {
			copyErr = errTooLong
			_, _ = io.Copy(io.Discard, stdout)
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return copyErr
}

func (g checkpointGit) hooksDir() string {
	if g.hooks == "" {
		return os.DevNull
	}
	_ = os.MkdirAll(g.hooks, 0o700)
	return g.hooks
}

var errTooLong = errors.New("output over the limit")

// changes are the files that differ between two checkpoints, with their line counts, and the
// unified diff, cut at proto.MaxDiffBytes on a line boundary.
func (g checkpointGit) changes(ctx context.Context, from, to string) ([]proto.FileChange, []byte, bool, error) {
	diffArgs := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--ignore-submodules=all"}
	var numstat bytes.Buffer
	if err := g.stream(ctx, "", nil, &numstat, 16<<20, append(append([]string{}, diffArgs...), "--numstat", "-z", from, to)...); err != nil && !errors.Is(err, errTooLong) {
		return nil, nil, false, err
	}
	statuses, err := g.run(ctx, "", nil, append(append([]string{}, diffArgs...), "--name-status", "-z", from, to)...)
	if err != nil {
		return nil, nil, false, err
	}
	files := parseChanges(numstat.String(), statuses)
	var diff bytes.Buffer
	truncated := false
	err = g.stream(ctx, "", nil, &diff, proto.MaxDiffBytes, append(append([]string{}, diffArgs...), from, to)...)
	if errors.Is(err, errTooLong) {
		truncated = true
		b := diff.Bytes()[:proto.MaxDiffBytes]
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			b = b[:i+1]
		}
		diff.Truncate(len(b))
	} else if err != nil {
		return nil, nil, false, err
	}
	return files, diff.Bytes(), truncated, nil
}

// parseChanges reads `git diff --numstat -z` and `--name-status -z` (no renames: one path each)
// into the files a turn changed, at most proto.MaxCheckpointFiles of them.
func parseChanges(numstat, statuses string) []proto.FileChange {
	status := map[string]string{}
	fields := strings.Split(statuses, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "" {
			break
		}
		status[fields[i+1]] = map[byte]string{'A': proto.FileAdded, 'D': proto.FileDeleted}[fields[i][0]]
		if status[fields[i+1]] == "" {
			status[fields[i+1]] = proto.FileModified
		}
	}
	var out []proto.FileChange
	for rec := range strings.SplitSeq(numstat, "\x00") {
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		f := proto.FileChange{Path: parts[2], Status: status[parts[2]]}
		if f.Status == "" {
			f.Status = proto.FileModified
		}
		if parts[0] == "-" && parts[1] == "-" {
			f.Binary = true
		} else {
			f.Added, _ = strconv.Atoi(parts[0])
			f.Removed, _ = strconv.Atoi(parts[1])
		}
		out = append(out, f)
		if len(out) == proto.MaxCheckpointFiles {
			break
		}
	}
	return out
}
