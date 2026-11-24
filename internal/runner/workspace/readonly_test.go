package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/onegator/gator/internal/proto"
)

// A read-only checkout is on no branch and cannot be written in, a second attempt of the job can
// replace it, and cleaning up takes it away whole.
func TestReadOnlyCheckout(t *testing.T) {
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
