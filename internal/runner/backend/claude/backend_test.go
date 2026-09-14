//go:build unix

package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
)

func fake(t *testing.T, fixture string) (Backend, string) {
	t.Helper()
	wd, _ := os.Getwd()
	args := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_CLAUDE_FIXTURE", filepath.Join(wd, "testdata", fixture))
	t.Setenv("FAKE_CLAUDE_ARGS", args)
	return Backend{Bin: filepath.Join(wd, "testdata", "fake-claude.sh"), GracePeriod: 200 * time.Millisecond}, args
}

func run(t *testing.T, b Backend, ctx context.Context, spec backend.Spec) (backend.Outcome, []string) {
	t.Helper()
	if spec.Dir == "" {
		spec.Dir = t.TempDir()
	}
	var evs []string
	out := b.Run(ctx, spec, func(typ string, _ any) { evs = append(evs, typ) })
	return out, evs
}

func TestSuccessfulRun(t *testing.T) {
	b, argsFile := fake(t, "claude-2.1.270-tool-success.ndjson")
	b.Model = "claude-opus-5"
	out, evs := run(t, b, context.Background(), backend.Spec{Prompt: "Do the thing"})
	if out.Status != proto.StatusDone || out.Summary != "DONE" || out.ToolCalls != 1 || out.SessionID == "" {
		t.Fatalf("outcome: %+v", out)
	}
	if out.Usage.CacheReadTokens == 0 || out.Usage.CostUSD <= 0 || out.Usage.DurationMS < 0 || out.Usage.Backend != "claude" {
		t.Fatalf("usage: %+v", out.Usage)
	}
	if strings.Join(evs, ",") != "rate_limit,session,tool_call,tool_result,text,result" {
		t.Fatalf("events: %v", evs)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"-p", "Do the thing", "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions", "--model", "claude-opus-5",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`} {
		if !strings.Contains(string(args), want+"\n") {
			t.Fatalf("missing arg %q in:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "--resume") {
		t.Fatal("fresh run must not resume")
	}
}

func TestResumePassesSession(t *testing.T) {
	b, argsFile := fake(t, "claude-2.1.270-tool-success.ndjson")
	run(t, b, context.Background(), backend.Spec{Prompt: "go on", SessionID: "sess-1"})
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--resume\nsess-1\n") {
		t.Fatalf("resume not passed:\n%s", args)
	}
}

func TestErrorResultFails(t *testing.T) {
	b, _ := fake(t, "claude-2.1.270-max-turns.ndjson")
	t.Setenv("FAKE_CLAUDE_EXIT", "1")
	out, _ := run(t, b, context.Background(), backend.Spec{Prompt: "x"})
	if out.Status != proto.StatusFailed || !strings.Contains(out.Reason, "error_max_turns") || out.Usage.CostUSD <= 0 {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestNoResultFails(t *testing.T) {
	b, _ := fake(t, "synthetic-no-result.ndjson")
	t.Setenv("FAKE_CLAUDE_EXIT", "3")
	out, _ := run(t, b, context.Background(), backend.Spec{Prompt: "x"})
	if out.Status != proto.StatusFailed || !strings.Contains(out.Reason, "without a result (exit 3)") {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestToolCallLimit(t *testing.T) {
	b, _ := fake(t, "synthetic-three-tools.ndjson")
	t.Setenv("FAKE_CLAUDE_SLEEP", "30")
	start := time.Now()
	out, _ := run(t, b, context.Background(), backend.Spec{Prompt: "x", MaxToolCalls: 2})
	if out.Status != proto.StatusFailed || out.Reason != "tool call limit (2) reached" {
		t.Fatalf("outcome: %+v", out)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("tool limit did not stop the process")
	}
}

func TestCancelKillsProcessGroup(t *testing.T) {
	b, _ := fake(t, "claude-2.1.270-tool-success.ndjson")
	pidFile := filepath.Join(t.TempDir(), "child")
	t.Setenv("FAKE_CLAUDE_CHILD", pidFile)
	t.Setenv("FAKE_CLAUDE_SLEEP", "30")
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel(errors.New("stopped: by test")) }()
	start := time.Now()
	out, _ := run(t, b, ctx, backend.Spec{Prompt: "x"})
	if !out.Interrupted || out.Reason != "stopped: by test" {
		t.Fatalf("outcome: %+v", out)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("cancel did not stop the run")
	}
	assertDead(t, pidFile)
}

// A background process that keeps stdout open must not keep the run alive.
func TestLingeringChildDoesNotHang(t *testing.T) {
	b, _ := fake(t, "claude-2.1.270-tool-success.ndjson")
	pidFile := filepath.Join(t.TempDir(), "child")
	t.Setenv("FAKE_CLAUDE_CHILD", pidFile)
	start := time.Now()
	out, _ := run(t, b, context.Background(), backend.Spec{Prompt: "x"})
	if out.Status != proto.StatusDone {
		t.Fatalf("outcome: %+v", out)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("run hung on a child holding stdout")
	}
	assertDead(t, pidFile)
}

func TestMissingBinary(t *testing.T) {
	out := Backend{Bin: "/nonexistent/claude"}.Run(context.Background(), backend.Spec{Dir: t.TempDir(), Prompt: "x"}, func(string, any) {})
	if out.Status != proto.StatusFailed || !strings.Contains(out.Reason, "start") {
		t.Fatalf("outcome: %+v", out)
	}
	if (Backend{Bin: "/nonexistent/claude"}).AuthState() != "missing" {
		t.Fatal("missing binary should report missing")
	}
}

func assertDead(t *testing.T, pidFile string) {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("background child %d survived", pid)
}

// Jobs must never see MCP servers or claude.ai connectors from the logged-in account; an
// operator can only grant an explicit set.
func TestMCPIsStrictByDefaultAndExplicitWhenConfigured(t *testing.T) {
	b, argsFile := fake(t, "claude-2.1.270-tool-success.ndjson")
	run(t, b, context.Background(), backend.Spec{Prompt: "x"})
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--strict-mcp-config\n--mcp-config\n{\"mcpServers\":{}}\n") {
		t.Fatalf("default must be strict and empty:\n%s", args)
	}
	_ = os.Remove(argsFile)
	b.MCPConfig = "/etc/gator/mcp.json"
	run(t, b, context.Background(), backend.Spec{Prompt: "x"})
	args, _ = os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--strict-mcp-config\n--mcp-config\n/etc/gator/mcp.json\n") {
		t.Fatalf("explicit config must still be strict:\n%s", args)
	}
}

// The final answer is the job's document; a brief longer than a screen must arrive whole.
func TestLongFinalAnswerIsKeptWhole(t *testing.T) {
	b, _ := fake(t, "synthetic-long-result.ndjson")
	out, _ := run(t, b, context.Background(), backend.Spec{Prompt: "x"})
	if out.Status != proto.StatusDone || len(out.Summary) < 6000 || !strings.HasSuffix(strings.TrimSpace(out.Summary), "end-of-document-marker") {
		t.Fatalf("summary cut: %d chars, ends %q", len(out.Summary), out.Summary[max(0, len(out.Summary)-40):])
	}
}
