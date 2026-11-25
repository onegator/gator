package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/runner/workspace"
)

// A backend that keeps its session open is not restarted by a steer: the message goes into the
// running turn, the worktree is checkpointed when each turn ends, and the job is one run.
func TestLiveBackendTakesTheSteerWithoutARestart(t *testing.T) {
	remote := originRepo(t)
	got := make(chan string, 1)
	be := &scripted{live: true, run: func(ctx context.Context, n int, spec backend.Spec, emit backend.Emit) backend.Outcome {
		if spec.Live == nil {
			t.Error("a live backend gets the live side of its session")
			return backend.Outcome{Status: proto.StatusFailed}
		}
		emit("tool_call", map[string]any{"name": "Edit"})
		_ = os.WriteFile(filepath.Join(spec.Dir, "api.go"), []byte("package api\n"), 0o644)
		select {
		case m := <-spec.Live.Steer:
			got <- m
		case <-time.After(5 * time.Second):
			t.Error("the steer never reached the run")
		}
		emit("result", map[string]any{})
		spec.Live.TurnEnded()
		return backend.Outcome{Status: proto.StatusDone, Summary: "done" + digestBlock, SessionID: "s", Usage: usage()}
	}}
	ex := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}}
	var mu sync.Mutex
	var sent []proto.Checkpoint
	steer := make(chan string, 1)
	io := client.JobIO{Steer: steer, Emit: func(string, any) {}, Checkpoint: func(cp proto.Checkpoint) { mu.Lock(); sent = append(sent, cp); mu.Unlock() }}
	go func() { time.Sleep(100 * time.Millisecond); steer <- "only the API" }()
	job := proto.Job{JobID: "job-live-1", TaskID: "t", Backend: "fake", Repo: &proto.Repo{Name: "app", URL: remote, DefaultBranch: "main"}}
	fin := ex.Run(context.Background(), job, io)
	if fin.Status != proto.StatusDone || len(be.specs) != 1 || <-got != "only the API" {
		t.Fatalf("finish %+v, %d runs", fin, len(be.specs))
	}
	if len(sent) != 1 || sent[0].Seq != 1 || len(sent[0].Files) != 1 {
		t.Fatalf("one checkpoint at the end of the turn: %+v", sent)
	}
}

// What a person says once the session ended is not lost: the session is resumed with it.
func TestLiveBackendResumesWithWhatCameAfter(t *testing.T) {
	steer := make(chan string, 1)
	be := &scripted{live: true, run: func(ctx context.Context, n int, spec backend.Spec, emit backend.Emit) backend.Outcome {
		if n == 1 {
			steer <- "and the docs" // said as the run ends, after the session stopped reading
			return backend.Outcome{Status: proto.StatusDone, Summary: "first", SessionID: "s7", Usage: usage(), Unread: []string{"and the tests"}}
		}
		return backend.Outcome{Status: proto.StatusDone, Summary: "second" + digestBlock, SessionID: "s7", Usage: usage()}
	}}
	ex := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}}
	io := client.JobIO{Steer: steer, Emit: func(string, any) {}}
	fin := ex.Run(context.Background(), proto.Job{JobID: "job-live-2", Backend: "fake"}, io)
	if fin.Status != proto.StatusDone || len(be.specs) != 2 || fin.Summary != "second" {
		t.Fatalf("finish %+v, %d runs", fin, len(be.specs))
	}
	if be.specs[1].SessionID != "s7" || !strings.Contains(be.specs[1].Prompt, "and the tests\n\nand the docs") {
		t.Fatalf("resumed with: %+v", be.specs[1])
	}
}
