package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/proto"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// origin creates a bare "remote" with one commit on main and returns its path.
func origin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	bare := filepath.Join(root, "origin.git")
	git(t, root, "init", "-q", "-b", "main", src)
	if err := os.WriteFile(filepath.Join(src, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", "init")
	git(t, root, "clone", "-q", "--bare", src, bare)
	return bare
}

func TestPrepareCommitPushCleanup(t *testing.T) {
	ctx := context.Background()
	remote := origin(t)
	m := &Manager{Root: t.TempDir()}
	job := proto.Job{JobID: "11111111-2222-3333-4444-555555555555", TaskID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Repo: &proto.Repo{Name: "app", URL: remote, DefaultBranch: "main"}}

	w, err := m.Prepare(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if w.Branch != "gator/aaaaaaaa-11111111" || w.Base == "" {
		t.Fatalf("workspace: %+v", w)
	}
	if _, err := os.Stat(filepath.Join(w.Dir, "README")); err != nil {
		t.Fatal("checkout missing README")
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "feature.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, w.Dir, "add", ".")
	git(t, w.Dir, "commit", "-q", "-m", "feature")
	if err := os.WriteFile(filepath.Join(w.Dir, "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	commits, changed, dirty, err := w.Changes(ctx)
	if err != nil || len(commits) != 1 || changed != 1 || dirty != 1 {
		t.Fatalf("changes: commits=%v changed=%d dirty=%d err=%v", commits, changed, dirty, err)
	}
	if err := w.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if got := git(t, remote, "rev-parse", "refs/heads/"+w.Branch); got != commits[0] {
		t.Fatalf("remote branch at %s, want %s", got, commits[0])
	}
	if got := git(t, remote, "branch", "--list"); !strings.Contains(got, "main") {
		t.Fatalf("pushing the job branch must not touch other remote branches: %s", got)
	}
	if err := w.Cleanup(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Dir); !os.IsNotExist(err) {
		t.Fatal("job directory left behind")
	}
}

func TestPrepareFetchesNewCommitsAndReusesCache(t *testing.T) {
	ctx := context.Background()
	remote := origin(t)
	m := &Manager{Root: t.TempDir()}
	repo := &proto.Repo{URL: remote, DefaultBranch: "main"}
	w1, err := m.Prepare(ctx, proto.Job{JobID: "job-one-000000", TaskID: "task", Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	// someone lands a commit on main in the remote
	work := t.TempDir()
	git(t, work, "clone", "-q", remote, "c")
	c := filepath.Join(work, "c")
	if err := os.WriteFile(filepath.Join(c, "NEW"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, c, "add", ".")
	git(t, c, "commit", "-q", "-m", "new")
	git(t, c, "push", "-q", "origin", "main")
	head := git(t, c, "rev-parse", "HEAD")

	w2, err := m.Prepare(ctx, proto.Job{JobID: "job-two-000000", TaskID: "task", Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if w2.Base != head || w1.Base == w2.Base {
		t.Fatalf("second job should start from the new main: w1=%s w2=%s want %s", w1.Base, w2.Base, head)
	}
	entries, _ := os.ReadDir(filepath.Join(m.Root, "repos"))
	if len(entries) != 1 {
		t.Fatalf("one cached clone per repository, got %d", len(entries))
	}
}

func TestNoRepoGivesEmptyDir(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	w, err := m.Prepare(context.Background(), proto.Job{JobID: "research-1"})
	if err != nil || w.HasRepo() {
		t.Fatalf("%+v %v", w, err)
	}
	if st, err := os.Stat(w.Dir); err != nil || !st.IsDir() {
		t.Fatal("no directory")
	}
	if _, _, _, err := w.Changes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Cleanup(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}

func TestRefusesOptionLikeURL(t *testing.T) {
	m := &Manager{Root: t.TempDir()}
	if _, err := m.Prepare(context.Background(), proto.Job{JobID: "x", Repo: &proto.Repo{URL: "--upload-pack=touch /tmp/pwned"}}); err == nil {
		t.Fatal("option-like url accepted")
	}
}
