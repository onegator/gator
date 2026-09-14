package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

// scripted is an in-process backend whose behaviour each test chooses.
type scripted struct {
	mu    sync.Mutex
	specs []backend.Spec
	run   func(ctx context.Context, n int, spec backend.Spec, emit backend.Emit) backend.Outcome
}

func (s *scripted) Name() string      { return "fake" }
func (s *scripted) AuthState() string { return "ok" }
func (s *scripted) Run(ctx context.Context, spec backend.Spec, emit backend.Emit) backend.Outcome {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	n := len(s.specs)
	s.mu.Unlock()
	return s.run(ctx, n, spec, emit)
}

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

func originRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	git(t, root, "init", "-q", "-b", "main", src)
	_ = os.WriteFile(filepath.Join(src, "README"), []byte("hi\n"), 0o644)
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", "init")
	bare := filepath.Join(root, "origin.git")
	git(t, root, "clone", "-q", "--bare", src, bare)
	return bare
}

func usage() proto.Usage {
	return proto.Usage{Backend: "fake", Model: "m", InputTokens: 10, OutputTokens: 5, DurationMS: 100, CostUSD: 0.01, CostEstimated: true, StartedAt: time.Now(), FinishedAt: time.Now()}
}

func jobIO() (client.JobIO, chan string, *[]string, *sync.Mutex) {
	steer := make(chan string, 4)
	var mu sync.Mutex
	evs := &[]string{}
	return client.JobIO{Emit: func(typ string, _ any) { mu.Lock(); *evs = append(*evs, typ); mu.Unlock() }, Steer: steer}, steer, evs, &mu
}

func TestCommitIsPushedAndWorkspaceCleaned(t *testing.T) {
	remote := originRepo(t)
	be := &scripted{run: func(_ context.Context, _ int, spec backend.Spec, emit backend.Emit) backend.Outcome {
		_ = os.WriteFile(filepath.Join(spec.Dir, "fix.go"), []byte("package x\n"), 0o644)
		c := exec.Command("git", "add", ".")
		c.Dir = spec.Dir
		_ = c.Run()
		c = exec.Command("git", "commit", "-q", "-m", "fix")
		c.Dir = spec.Dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@a", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@a")
		if out, err := c.CombinedOutput(); err != nil {
			t.Errorf("commit: %v %s", err, out)
		}
		emit("text", map[string]string{"text": "fixed"})
		return backend.Outcome{Status: proto.StatusDone, Summary: "fixed it", SessionID: "s1", Usage: usage()}
	}}
	ex := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}, Push: true}
	io, _, evs, _ := jobIO()
	job := proto.Job{JobID: "job-aaaa-1111", TaskID: "task-bbbb-2222", TaskTitle: "Fix login", Role: "worker", Phase: "implementation",
		Backend: "fake", Instruction: "Fix the login bug.", Repo: &proto.Repo{Name: "app", URL: remote, DefaultBranch: "main"}}
	fin := ex.Run(context.Background(), job, io)
	if fin.Status != proto.StatusDone || len(fin.Commits) != 1 || fin.ChangedFiles != 1 || fin.Branch == "" || fin.Summary != "fixed it" || fin.SessionID != "s1" {
		t.Fatalf("finish: %+v", fin)
	}
	if got := git(t, remote, "rev-parse", "refs/heads/"+fin.Branch); got != fin.Commits[0] {
		t.Fatalf("branch not pushed: %s", got)
	}
	if !strings.Contains(be.specs[0].Prompt, "Fix the login bug.") || !strings.Contains(be.specs[0].Prompt, `"Fix login"`) || !strings.Contains(be.specs[0].Prompt, "worker") {
		t.Fatalf("prompt: %s", be.specs[0].Prompt)
	}
	if _, err := os.Stat(be.specs[0].Dir); !os.IsNotExist(err) {
		t.Fatal("workspace not cleaned up")
	}
	if strings.Join(*evs, ",") != "workspace,text,git" {
		t.Fatalf("events: %v", *evs)
	}
}

func TestSteerResumesSession(t *testing.T) {
	be := &scripted{run: func(ctx context.Context, n int, spec backend.Spec, emit backend.Emit) backend.Outcome {
		if n == 1 {
			emit("text", map[string]string{"text": "working"})
			<-ctx.Done()
			return backend.Outcome{Interrupted: true, Cause: context.Cause(ctx), SessionID: "sess-9", Usage: usage()}
		}
		return backend.Outcome{Status: proto.StatusDone, Summary: "done after steer", SessionID: "sess-9", Usage: usage()}
	}}
	ex := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}}
	io, steer, _, _ := jobIO()
	go func() { time.Sleep(100 * time.Millisecond); steer <- "only touch the API layer" }()
	fin := ex.Run(context.Background(), proto.Job{JobID: "j", Backend: "fake"}, io)
	if fin.Status != proto.StatusDone || len(be.specs) != 2 {
		t.Fatalf("finish %+v runs %d", fin, len(be.specs))
	}
	if be.specs[1].SessionID != "sess-9" || !strings.Contains(be.specs[1].Prompt, "only touch the API layer") {
		t.Fatalf("second run should resume with the correction: %+v", be.specs[1])
	}
	if fin.Usage.InputTokens != 20 || fin.Usage.DurationMS != 200 {
		t.Fatalf("usage should add up across runs: %+v", fin.Usage)
	}
}

func TestStopAndTimeout(t *testing.T) {
	block := &scripted{run: func(ctx context.Context, _ int, _ backend.Spec, _ backend.Emit) backend.Outcome {
		<-ctx.Done()
		return backend.Outcome{Interrupted: true, Cause: context.Cause(ctx), Usage: usage()}
	}}
	ex := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": block}}

	ctx, cancel := context.WithCancelCause(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel(errors.New("stopped: wrong approach")) }()
	io, _, _, _ := jobIO()
	if fin := ex.Run(ctx, proto.Job{JobID: "j1", Backend: "fake"}, io); fin.Status != proto.StatusStopped || fin.StopReason != "stopped: wrong approach" {
		t.Fatalf("stop: %+v", fin)
	}

	tctx, tcancel := context.WithTimeoutCause(context.Background(), 50*time.Millisecond, errors.New("job timeout"))
	defer tcancel()
	io2, _, _, _ := jobIO()
	if fin := ex.Run(tctx, proto.Job{JobID: "j2", Backend: "fake"}, io2); fin.Status != proto.StatusFailed || fin.StopReason != "job timeout" {
		t.Fatalf("timeout: %+v", fin)
	}
}

func TestUnknownBackend(t *testing.T) {
	ex := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{}}
	io, _, _, _ := jobIO()
	if fin := ex.Run(context.Background(), proto.Job{JobID: "j", Backend: "codex"}, io); fin.Status != proto.StatusFailed || !strings.Contains(fin.StopReason, "codex") {
		t.Fatalf("%+v", fin)
	}
}

func TestPromptWithGuideAndContext(t *testing.T) {
	job := proto.Job{Role: "planner", Phase: "planning", TaskTitle: "Dark mode", TaskDescription: "Users want dark mode.",
		Guide: "# Role: planner\nWrite the plan.", Instruction: "Keep it short.",
		Context: []proto.ContextDoc{{Kind: "rollback", Title: "Why this phase was sent back", Body: "missed the migration"}, {Kind: "brief", Title: "brief from discovery (v1, approved)", Body: "## Problem\nX"}}}
	p := Prompt(job, true)
	for _, want := range []string{"# Role: planner", `Task: "Dark mode" (phase: planning, your role: planner)`, "Users want dark mode.", "Keep it short.",
		"## Context: Why this phase was sent back", "missed the migration", "## Context: brief from discovery (v1, approved)", "the runner pushes that branch"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	// A read-only role (researcher, planner, reviewer) must not be told to commit by the framing.
	if strings.Contains(p, "Commit your changes") {
		t.Error("with a role guide the framing must not tell the agent to commit")
	}
	if strings.Contains(p, "Finish with a short summary") {
		t.Error("a role guide defines the output; the generic closing line must not compete with it")
	}
}
