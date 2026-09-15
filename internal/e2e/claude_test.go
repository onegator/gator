//go:build unix

package e2e

import (
	"context"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/backend/claude"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/runner/executor"
	"github.com/onegator/gator/internal/runner/workspace"
	"github.com/onegator/gator/internal/server/api/gen"
)

func gitCmd(t *testing.T, dir string, args ...string) string {
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

// A job for a project with a repository runs Claude Code (replayed from a real recording)
// in a worktree, commits, pushes its branch and reports usage into the task's metrics.
func TestClaudeJobCommitsPushesAndReportsUsage(t *testing.T) {
	h := newHarness(t)
	task := h.task()

	root := t.TempDir()
	src := filepath.Join(root, "src")
	gitCmd(t, root, "init", "-q", "-b", "main", src)
	_ = os.WriteFile(filepath.Join(src, "README"), []byte("app\n"), 0o644)
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-q", "-m", "init")
	origin := filepath.Join(root, "origin.git")
	gitCmd(t, root, "clone", "-q", "--bare", src, origin)

	def, primary := "main", true
	if code := h.do("PUT", "/projects/"+task.ProjectId.String()+"/repos", gen.ProjectRepo{Name: "app", Url: origin, DefaultBranch: &def, Primary: &primary}, nil); code != 200 {
		t.Fatalf("add repo: %d", code)
	}

	wd, _ := os.Getwd()
	td := filepath.Join(wd, "..", "runner", "backend", "claude", "testdata")
	t.Setenv("FAKE_CLAUDE_FIXTURE", filepath.Join(td, "claude-2.1.270-tool-success.ndjson"))
	t.Setenv("FAKE_CLAUDE_COMMIT", "1")
	// Without a login the runner reports claude as missing (as on a clean CI machine) and the
	// server rightly leases it nothing; the fake binary needs no real token.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fake-token-for-tests")
	// The recording has no digest, so the executor resumes the session once to ask for it.
	t.Setenv("FAKE_CLAUDE_RESUME_FIXTURE", filepath.Join(td, "synthetic-digest-reply.ndjson"))
	ex := &executor.Executor{
		WS:       &workspace.Manager{Root: t.TempDir()},
		Backends: map[string]backend.Backend{"claude": claude.Backend{Bin: filepath.Join(td, "fake-claude.sh")}},
		Push:     true,
	}
	c := client.New(client.Config{ServerURL: h.srv.URL, Token: h.runner, Name: "e2e-claude", Location: "other",
		Backends: []string{"claude"}, Projects: []string{task.ProjectId.String()}, MaxParallel: 1,
		HeartbeatInterval: 100 * time.Millisecond, MinBackoff: 50 * time.Millisecond}, ex, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	job := h.job(task, "claude", "Make the change.", 0)
	done := h.waitJob(job.Id.String(), "done", "failed")
	if done.Status != "done" {
		t.Fatalf("job %s: %s", done.Status, deref(done.StopReason))
	}
	r := *done.Receipt
	branch, _ := r["branch"].(string)
	commits, _ := r["commits"].([]any)
	if !strings.HasPrefix(branch, "gator/") || len(commits) != 1 || r["changed_files"] != float64(1) || r["summary"] != "DONE" {
		t.Fatalf("receipt: %+v", r)
	}
	if d, _ := r["digest"].(map[string]any); d == nil || d["changes"].([]any)[0] != "added gator.txt" {
		t.Fatalf("receipt digest: %+v", r["digest"])
	}
	if got := gitCmd(t, origin, "rev-parse", "refs/heads/"+branch); got != commits[0] {
		t.Fatalf("origin has %s, want %s", got, commits[0])
	}

	var evs []gen.JobEvent
	h.do("GET", "/jobs/"+job.Id.String()+"/events?limit=100", nil, &evs)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Type)
	}
	if got := strings.Join(kinds, ","); got != "workspace,rate_limit,session,tool_call,tool_result,text,result,digest_requested,session,result,git" {
		t.Fatalf("event stream: %s", got)
	}

	var m gen.TaskMetrics
	h.do("GET", "/tasks/"+task.Id.String()+"/metrics", nil, &m)
	// Sum over every model in the recording's modelUsage (Opus plus the background Haiku call),
	// plus the small digest round (5 in, 7 out, $0.001).
	if m.Tokens.Input != 951 || m.Tokens.Output != 160 || m.Tokens.CacheRead != 39662 || m.Tokens.CacheWrite != 24069 {
		t.Fatalf("tokens: %+v", m.Tokens)
	}
	if math.Abs(m.CostUsd-0.266168) > 1e-6 || !m.CostEstimated || m.Jobs != 1 {
		t.Fatalf("cost %v estimated %v jobs %d", m.CostUsd, m.CostEstimated, m.Jobs)
	}
}
