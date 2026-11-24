package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/onegator/gator/internal/proto"
)

// A read-only checkout is on no branch and cannot be written in, a second attempt of the job can
// replace it, and cleaning up takes it away whole.
func TestReadOnlyCheckout(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes wherever it likes; file modes prove nothing here")
	}
	ctx := context.Background()
	remote := origin(t)
	m := &Manager{Root: t.TempDir()}
	job := proto.Job{JobID: "ro-1", TaskID: "task", Repo: &proto.Repo{Name: "app", URL: remote, DefaultBranch: "main"}}
	for attempt := range 2 {
		w, err := m.PrepareReadOnly(ctx, job)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if !w.ReadOnly || w.Branch != "" || w.Base == "" || !w.HasRepo() {
			t.Fatalf("workspace: %+v", w)
		}
		if err := os.WriteFile(filepath.Join(w.Dir, "README"), []byte("x"), 0o644); err == nil {
			t.Fatal("a file of a read-only checkout was written")
		}
		if err := os.WriteFile(filepath.Join(w.Dir, "new.txt"), []byte("x"), 0o644); err == nil {
			t.Fatal("a file was added to a read-only checkout")
		}
		if commits, _, dirty, err := w.Changes(ctx); err != nil || len(commits) != 0 || dirty != 0 {
			t.Fatalf("changes: %v %d %v", commits, dirty, err)
		}
		if env, err := w.SignCommits("Co-Authored-By: @x via Gator"); err != nil || env != nil {
			t.Fatalf("a read-only checkout signs nothing: %v %v", env, err)
		}
		if err := w.Push(ctx); err != nil {
			t.Fatal(err)
		}
		commit := exec.Command("git", "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-q", "--allow-empty", "-m", "sneak")
		commit.Dir = w.Dir
		if out, err := commit.CombinedOutput(); err == nil {
			t.Fatalf("git committed in a read-only checkout: %s", out)
		}
		if attempt == 1 {
			if err := w.Cleanup(ctx, false); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(w.Dir); !os.IsNotExist(err) {
				t.Fatalf("cleanup left %s", w.Dir)
			}
		}
	}
	// Without a repository it is an empty directory nobody writes in.
	w, err := m.PrepareReadOnly(ctx, proto.Job{JobID: "ro-2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "a"), []byte("x"), 0o644); err == nil {
		t.Fatal("an empty read-only directory was written in")
	}
	if err := w.Cleanup(ctx, false); err != nil {
		t.Fatal(err)
	}
}

// A read-only job reads the branch the task's work was pushed to, when the remote has it.
func TestReadOnlyCheckoutReadsTheTasksBranch(t *testing.T) {
	ctx := context.Background()
	remote := origin(t)
	work := t.TempDir()
	git(t, work, "clone", "-q", remote, "w")
	clone := filepath.Join(work, "w")
	git(t, clone, "checkout", "-q", "-b", "gator/task-1")
	_ = os.WriteFile(filepath.Join(clone, "fix.go"), []byte("package fix\n"), 0o644)
	git(t, clone, "add", ".")
	git(t, clone, "commit", "-q", "-m", "fix")
	git(t, clone, "push", "-q", "origin", "gator/task-1")

	m := &Manager{Root: t.TempDir()}
	for _, tc := range []struct {
		branch, read string
		has          bool
	}{{"gator/task-1", "gator/task-1", true}, {"gator/gone", "main", false}, {"", "main", false}} {
		w, err := m.PrepareReadOnly(ctx, proto.Job{JobID: "ro-" + tc.read + tc.branch[len(tc.branch)/2:], TaskID: "t",
			Repo: &proto.Repo{Name: "app", URL: remote, DefaultBranch: "main", ReadBranch: tc.branch}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = os.Stat(filepath.Join(w.Dir, "fix.go"))
		if w.Read != tc.read || (err == nil) != tc.has {
			t.Fatalf("%q: reads %s, has the fix %v", tc.branch, w.Read, err == nil)
		}
		if err := w.Cleanup(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
}
