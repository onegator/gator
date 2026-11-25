package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/runner/workspace"
)

// The files a reply is handed are fetched with the job's own identity into a directory of the
// runner, outside the worktree: an image is shown as one, a script is a path and never runs, the
// agent is told where they are, and they are gone with the job.
func TestAttachmentsAreFetchedOutsideTheWorktree(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 32)...)
	files := map[string][]byte{"a1": png, "a2": []byte("#!/bin/sh\nrm -rf ~\n"), "a3": []byte("hello")}
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		b, ok := files[strings.TrimPrefix(r.URL.Path, proto.AttachmentPath)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	var seen []backend.Attachment
	var dir string
	be := &scripted{run: func(_ context.Context, _ int, spec backend.Spec, _ backend.Emit) backend.Outcome {
		seen, dir = spec.Attachments, spec.Dir
		if !strings.Contains(spec.Prompt, "## Files people sent in the conversation") {
			t.Errorf("the prompt does not say where the files are:\n%s", spec.Prompt)
		}
		return backend.Outcome{Status: proto.StatusDone, Summary: "ok" + digestBlock, Usage: usage()}
	}}
	root := t.TempDir()
	ex := &Executor{WS: &workspace.Manager{Root: root}, Backends: map[string]backend.Backend{"fake": be}, ServerURL: srv.URL, CLIPath: "/bin/gator-cli"}
	job := proto.Job{JobID: "job-files", Backend: "fake", Kind: proto.KindReply, Tier: proto.TierChat, AgentToken: "gtr_job_x",
		Attachments: []proto.Attachment{{ID: "a1", Name: "crash.png"}, {ID: "a2", Name: "fix.sh"}, {ID: "a3", Name: "../../notes.txt"}, {ID: "gone", Name: "x"}}}
	var events []string
	fin := ex.Run(context.Background(), job, client.JobIO{Emit: func(typ string, _ any) { events = append(events, typ) }, Steer: make(chan string)})
	if fin.Status != proto.StatusDone || len(seen) != 3 {
		t.Fatalf("finish %+v, files %+v", fin, seen)
	}
	for _, a := range auth {
		if a != "Bearer gtr_job_x" {
			t.Fatalf("fetched as %q", a)
		}
	}
	if !seen[0].Image || seen[0].MediaType != "image/png" || seen[1].Image || seen[2].Image {
		t.Fatalf("images: %+v", seen)
	}
	for _, f := range seen {
		if rel, err := filepath.Rel(dir, f.Path); err == nil && !strings.HasPrefix(rel, "..") {
			t.Fatalf("%s is inside the worktree", f.Path)
		}
		if !strings.HasPrefix(f.Path, filepath.Join(root, "attachments")) || strings.Contains(filepath.Base(f.Path), "..") {
			t.Fatalf("path: %s", f.Path)
		}
		if _, err := os.Stat(f.Path); !os.IsNotExist(err) {
			t.Fatalf("%s is left after the job", f.Path)
		}
	}
	if !strings.Contains(strings.Join(events, ","), "attachment_skipped") {
		t.Fatalf("a file that could not be fetched is not said: %v", events)
	}
}

func TestRunnableFilesAreNeverImages(t *testing.T) {
	for _, c := range []struct {
		name string
		head string
		want bool
	}{{"x.png", "\x7fELF....", true}, {"x.sh", "hello", true}, {"x.png", "\x89PNG", false}, {"MZ.png", "MZ", true}} {
		if got := runs(c.name, []byte(c.head)); got != c.want {
			t.Errorf("%s %q: %v", c.name, c.head, got)
		}
	}
	if got := safeName("../../etc/pass wd"); got != "pass_wd" {
		t.Fatalf("safe name: %q", got)
	}
}
