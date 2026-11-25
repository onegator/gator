package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/proto"
)

func checkpointWorkspace(t *testing.T) (*Workspace, *Manager) {
	t.Helper()
	m := &Manager{Root: t.TempDir()}
	job := proto.Job{JobID: "cccccccc-2222-3333-4444-555555555555", TaskID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Repo: &proto.Repo{Name: "app", URL: origin(t), DefaultBranch: "main"}}
	w, err := m.Prepare(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Cleanup(context.Background(), false) })
	return w, m
}

func checkpoint(t *testing.T, w *Workspace, exclude ...string) *CheckpointResult {
	t.Helper()
	cp, err := w.Checkpoint(context.Background(), exclude)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A turn's diff is what that turn did to the files, committed or not, and taking it leaves the
// agent's branch, HEAD and index exactly as they were.
func TestCheckpointDiffsATurnWithoutTouchingTheAgentsGit(t *testing.T) {
	w, _ := checkpointWorkspace(t)
	if cp := checkpoint(t, w); cp.Seq != 0 || len(cp.Files) != 0 || cp.HeadCommit == "" {
		t.Fatalf("baseline: %+v", cp)
	}

	// Turn 1: the agent commits one file and leaves another staged but uncommitted.
	write(t, filepath.Join(w.Dir, "fix.go"), "package x\n\nfunc Fix() {}\n")
	git(t, w.Dir, "add", "fix.go")
	git(t, w.Dir, "commit", "-q", "-m", "fix")
	write(t, filepath.Join(w.Dir, "notes.md"), "draft\n")
	git(t, w.Dir, "add", "notes.md")
	head := git(t, w.Dir, "rev-parse", "HEAD")
	branch := git(t, w.Dir, "rev-parse", "--abbrev-ref", "HEAD")
	staged := git(t, w.Dir, "diff", "--cached", "--name-only")

	cp := checkpoint(t, w)
	if cp.Seq != 1 || cp.BaseCommit == "" || cp.HeadCommit == cp.BaseCommit || len(cp.Files) != 2 {
		t.Fatalf("turn 1: %+v", cp.Checkpoint)
	}
	if !strings.Contains(string(cp.Diff), "+func Fix() {}") || !strings.Contains(string(cp.Diff), "+draft") {
		t.Fatalf("diff: %s", cp.Diff)
	}
	for _, f := range cp.Files {
		if f.Status != proto.FileAdded || f.Removed != 0 || f.Added == 0 {
			t.Fatalf("file: %+v", f)
		}
	}
	if git(t, w.Dir, "rev-parse", "HEAD") != head || git(t, w.Dir, "rev-parse", "--abbrev-ref", "HEAD") != branch ||
		git(t, w.Dir, "diff", "--cached", "--name-only") != staged {
		t.Fatal("the checkpoint moved the agent's HEAD, branch or index")
	}
	if got := git(t, w.Dir, "status", "--porcelain"); got != "A  notes.md" {
		t.Fatalf("status after the checkpoint: %q", got)
	}

	// Turn 2 changes one line; its diff is that line, the job's is everything.
	write(t, filepath.Join(w.Dir, "README"), "hello\n")
	cp = checkpoint(t, w)
	if cp.Seq != 2 || len(cp.Files) != 1 || cp.Files[0].Path != "README" || cp.Files[0].Status != proto.FileModified ||
		cp.Files[0].Added != 1 || cp.Files[0].Removed != 1 {
		t.Fatalf("turn 2: %+v", cp.Files)
	}
	if strings.Contains(string(cp.Diff), "Fix()") {
		t.Fatalf("turn 2 carries turn 1: %s", cp.Diff)
	}
	if len(cp.WholeFiles) != 3 || !strings.Contains(string(cp.Whole), "Fix()") {
		t.Fatalf("whole job: %+v", cp.WholeFiles)
	}
	if s := cp.Summary(); s.Files != 1 || s.Added != 1 || s.Removed != 1 {
		t.Fatalf("summary: %+v", s)
	}
	// Hidden refs, never on a branch.
	if refs := git(t, w.bare, "for-each-ref", "--format=%(refname)", "refs/gator/"); strings.Count(refs, "\n") != 2 {
		t.Fatalf("checkpoint refs: %q", refs)
	}
	if branches := git(t, w.bare, "branch", "--contains", cp.HeadCommit); branches != "" {
		t.Fatalf("a checkpoint is on a branch: %q", branches)
	}
}

// Nothing the agent could have written into the repository runs a program when the runner takes
// a checkpoint, and nothing outside the job's worktree gets into it: not through a filter, not
// through core.worktree, not through a link, not through the worktree's .git file.
func TestCheckpointRunsNothingAndReadsNothingOutsideTheWorktree(t *testing.T) {
	w, m := checkpointWorkspace(t)
	checkpoint(t, w)

	outside := filepath.Join(m.Root, "secret.txt")
	write(t, outside, "TOP-SECRET\n")
	ran := filepath.Join(t.TempDir(), "ran")
	// A clean filter, declared where the agent can write: the repository's config and attributes.
	git(t, w.Dir, "config", "filter.evil.clean", "touch "+ran+"; cat")
	git(t, w.Dir, "config", "filter.evil.required", "true")
	// A driver whose name has "=" in it, which `git -c` would split at.
	git(t, w.Dir, "config", "filter.a=b.clean", "touch "+ran+"; cat")
	write(t, filepath.Join(w.bare, "info", "attributes"), "* filter=evil\n*.txt filter=a=b\n")
	write(t, filepath.Join(w.Dir, ".gitattributes"), "* filter=evil\n")
	write(t, filepath.Join(w.gitDir, "info", "attributes"), "* filter=evil\n")
	// The worktree pointed elsewhere, and fsmonitor and a hook asked to run.
	git(t, w.Dir, "config", "core.fsmonitor", "touch "+ran+";")
	write(t, filepath.Join(w.bare, "hooks", "reference-transaction"), "#!/bin/sh\ntouch "+ran+"\n")
	_ = os.Chmod(filepath.Join(w.bare, "hooks", "reference-transaction"), 0o755)
	git(t, w.Dir, "config", "core.worktree", m.Root)
	// A link to a file outside, and a file the runner says never goes in.
	if err := os.Symlink(outside, filepath.Join(w.Dir, "link")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.Dir, ".gator-token"), "runner-token\n")
	write(t, filepath.Join(w.Dir, "change.txt"), "a change\n")
	// The worktree's .git file pointed at another repository.
	if err := os.WriteFile(filepath.Join(w.Dir, ".git"), []byte("gitdir: "+filepath.Join(m.Root, "nowhere")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cp := checkpoint(t, w, filepath.Join(w.Dir, ".gator-token"), outside)
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("a checkpoint ran a program the agent configured")
	}
	all := string(cp.Diff)
	if strings.Contains(all, "TOP-SECRET") || strings.Contains(all, "runner-token") {
		t.Fatalf("the checkpoint carries what is outside the worktree:\n%s", all)
	}
	paths := map[string]bool{}
	for _, f := range cp.Files {
		paths[f.Path] = true
	}
	if !paths["change.txt"] || !paths["link"] || paths[".gator-token"] || paths["secret.txt"] {
		t.Fatalf("files: %+v", cp.Files)
	}
	if !strings.Contains(all, "+"+outside) {
		t.Fatalf("a link is its target's path, not its content:\n%s", all)
	}
}

// A binary file is listed as changed and nothing of it is in the diff; a diff over the limit is
// cut at a line and says so.
func TestCheckpointBinaryAndTruncation(t *testing.T) {
	w, _ := checkpointWorkspace(t)
	checkpoint(t, w)
	if err := os.WriteFile(filepath.Join(w.Dir, "logo.png"), []byte("\x89PNG\x00\x01\x02binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 99) + "\n"
	write(t, filepath.Join(w.Dir, "big.txt"), strings.Repeat(line, proto.MaxDiffBytes/100+500))
	cp := checkpoint(t, w)
	var png *proto.FileChange
	for i, f := range cp.Files {
		if f.Path == "logo.png" {
			png = &cp.Files[i]
		}
	}
	if png == nil || !png.Binary || png.Added != 0 {
		t.Fatalf("binary: %+v", cp.Files)
	}
	if !cp.Truncated || len(cp.Diff) > proto.MaxDiffBytes || cp.Diff[len(cp.Diff)-1] != '\n' {
		t.Fatalf("truncation: %v %d", cp.Truncated, len(cp.Diff))
	}
	if strings.Contains(string(cp.Diff), "binary") {
		t.Fatal("the binary's bytes are in the diff")
	}
}

// Checkpoints go with the worktree: their refs and their cached index.
func TestCleanupDropsCheckpoints(t *testing.T) {
	w, _ := checkpointWorkspace(t)
	checkpoint(t, w)
	write(t, filepath.Join(w.Dir, "a.txt"), "a\n")
	checkpoint(t, w)
	index := w.checkpointIndex()
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("no cached index: %v", err)
	}
	if err := w.Cleanup(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if refs := git(t, w.bare, "for-each-ref", "refs/gator/"); refs != "" {
		t.Fatalf("refs left: %q", refs)
	}
	if _, err := os.Stat(index); !os.IsNotExist(err) {
		t.Fatal("the cached index is left")
	}
}

// A read-only checkout has nothing to checkpoint.
func TestReadOnlyCheckoutHasNoCheckpoints(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	job := proto.Job{JobID: "dddddddd-2222-3333-4444-555555555555", TaskID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Tier: proto.TierChat,
		Repo: &proto.Repo{Name: "app", URL: origin(t), DefaultBranch: "main"}}
	w, err := m.PrepareReadOnly(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Cleanup(context.Background(), false)
	if cp, err := w.Checkpoint(context.Background(), nil); err != nil || cp != nil {
		t.Fatalf("read-only checkpoint: %+v %v", cp, err)
	}
}

// Git never reads a file of the worktree for a checkpoint, so no driver runs whenever it is
// configured — before the checkpoint or while it runs — and a tracked directory swapped for a link
// to somewhere else is not followed.
func TestCheckpointReadsFilesItselfInsideTheWorktree(t *testing.T) {
	w, m := checkpointWorkspace(t)
	write(t, filepath.Join(w.Dir, "conf", "app.txt"), "v1\n")
	git(t, w.Dir, "add", "conf/app.txt")
	git(t, w.Dir, "commit", "-q", "-m", "conf")
	checkpoint(t, w)

	ran := filepath.Join(t.TempDir(), "ran")
	// Every driver git could be told of, under every name, required: if git read a file it would run.
	write(t, filepath.Join(w.gitDir, "info", "attributes"), "* filter=late diff=late\n")
	write(t, filepath.Join(w.bare, "info", "attributes"), "* filter=late\n")
	git(t, w.bare, "config", "filter.late.clean", "touch "+ran+"; cat")
	git(t, w.bare, "config", "filter.late.required", "true")
	git(t, w.bare, "config", "diff.late.textconv", "touch "+ran+"; cat")
	git(t, w.bare, "config", "diff.late.command", "touch "+ran)

	secret := filepath.Join(m.Root, "elsewhere")
	write(t, filepath.Join(secret, "app.txt"), "TOP-SECRET\n")
	if err := os.RemoveAll(filepath.Join(w.Dir, "conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(w.Dir, "conf")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.Dir, "new.txt"), "new\n")
	_ = os.Chmod(filepath.Join(w.Dir, "new.txt"), 0o755)

	cp := checkpoint(t, w)
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("a checkpoint ran a driver of the repository's")
	}
	if strings.Contains(string(cp.Diff), "TOP-SECRET") {
		t.Fatalf("a link led outside the worktree:\n%s", cp.Diff)
	}
	byPath := map[string]proto.FileChange{}
	for _, f := range cp.Files {
		byPath[f.Path] = f
	}
	if byPath["new.txt"].Status != proto.FileAdded || byPath["conf"].Status != proto.FileAdded || byPath["conf/app.txt"].Status != proto.FileDeleted {
		t.Fatalf("files: %+v", cp.Files)
	}
	if !strings.Contains(string(cp.Diff), "new file mode 100755") || !strings.Contains(string(cp.Diff), "new file mode 120000") {
		t.Fatalf("modes:\n%s", cp.Diff)
	}
	// The blobs are git's own: the repository reads them.
	if got := git(t, w.bare, "cat-file", "-p", cp.HeadCommit+":new.txt"); got != "new" {
		t.Fatalf("blob: %q", got)
	}
}
