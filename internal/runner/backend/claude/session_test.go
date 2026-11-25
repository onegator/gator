//go:build unix

package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/backend/replay"
)

// liveFake is the fake CLI playing a session script; stdin is where it writes what it read.
func liveFake(t *testing.T, script string) (Backend, string, string) {
	t.Helper()
	b, args := fake(t, "claude-2.1.270-tool-success.ndjson")
	wd, _ := os.Getwd()
	stdin := filepath.Join(t.TempDir(), "stdin")
	t.Setenv("FAKE_CLAUDE_SESSION", filepath.Join(wd, "testdata", script))
	t.Setenv("FAKE_CLAUDE_STDIN", stdin)
	b.Mode = ModeSession
	return b, args, stdin
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, _ := os.ReadFile(path)
	var out []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// A person's message goes into the running turn: one process, one session, one init, the steer
// written as a user line and read mid-turn, and the run ends when that turn does.
func TestSessionSteersWithoutARestart(t *testing.T) {
	b, argsFile, stdin := liveFake(t, "synthetic-session-steer.ndjson")
	steer := make(chan string, 1)
	rec := &replay.Recorder{}
	var applied atomic.Int32
	emit := func(typ string, payload any) {
		switch typ {
		case "tool_call": // the person speaks while the agent works
			steer <- "only the API layer, please"
		case "steer_applied": // said by the session's writer, in its own time: counted apart
			applied.Add(1)
			return
		}
		rec.Emit(typ, payload)
	}
	var turns atomic.Int32
	out := b.Run(context.Background(), backend.Spec{Dir: t.TempDir(), Prompt: "fix the totals",
		Live: &backend.Live{Steer: steer, TurnEnded: func() { turns.Add(1) }}}, emit)
	if out.Status != proto.StatusDone || out.Summary != "Changed the API layer only." || out.SessionID != "00000000-0000-4000-8000-0000000000f1" || out.ToolCalls != 1 {
		t.Fatalf("outcome: %+v", out)
	}
	if turns.Load() != 1 || len(out.Unread) != 0 {
		t.Fatalf("turns %d, unread %v", turns.Load(), out.Unread)
	}
	if got := rec.Types(); strings.Count(got, "session") != 1 || applied.Load() != 1 {
		t.Fatalf("events: %s, %d steers applied", got, applied.Load())
	}
	args, _ := os.ReadFile(argsFile)
	if strings.Count(string(args), "---") != 1 {
		t.Fatalf("one process, not a restart:\n%s", args)
	}
	for _, want := range []string{"--input-format\nstream-json\n", "--output-format\nstream-json\n", "--replay-user-messages\n",
		"--permission-mode\nbypassPermissions\n", "--permission-prompts\nnone\n"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("missing %q in:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "fix the totals") {
		t.Fatal("the prompt goes on stdin, not on the command line")
	}
	lines := readLines(t, stdin)
	if len(lines) != 2 || lines[0]["type"] != "user" || !strings.Contains(mustJSON(lines[1]), "only the API layer, please") {
		t.Fatalf("stdin: %+v", lines)
	}
	replay.Compare(t, filepath.Join("testdata", "golden", "synthetic-session-steer.golden.json"), rec.Events(), sessionOutcome(out))
}

// Stop is an interrupt: the turn ends within InterruptWait, the process exits by itself once its
// input closes, and the run says it was interrupted.
func TestSessionStopIsAnInterrupt(t *testing.T) {
	b, _, stdin := liveFake(t, "synthetic-session-interrupt.ndjson")
	b.GracePeriod = 10 * time.Second // a kill would show as a run longer than the interrupt
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel(errors.New("stopped: by a person")) }()
	start := time.Now()
	rec := &replay.Recorder{}
	out := b.Run(ctx, backend.Spec{Dir: t.TempDir(), Prompt: "go", Live: &backend.Live{}}, rec.Emit)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("stop took %s", took)
	}
	if !out.Interrupted || out.Reason != "stopped: by a person" || out.SessionID == "" {
		t.Fatalf("outcome: %+v", out)
	}
	if !strings.Contains(mustJSON(readLines(t, stdin)), `"subtype":"interrupt"`) {
		t.Fatal("no interrupt request was sent")
	}
	replay.Compare(t, filepath.Join("testdata", "golden", "synthetic-session-interrupt.golden.json"), rec.Events(), sessionOutcome(out))
}

// recordingApprover answers by tool and records what it was asked.
type recordingApprover struct {
	mu    sync.Mutex
	asked []backend.ToolRequest
}

func (a *recordingApprover) Decide(_ context.Context, r backend.ToolRequest) backend.Decision {
	a.mu.Lock()
	a.asked = append(a.asked, r)
	a.mu.Unlock()
	if r.Kind == backend.RequestQuestion {
		return backend.Decision{Answer: "Postgres"}
	}
	return backend.Decision{Message: "Ada: not from the internet, use the vendored copy"}
}

// The CLI asks before a tool and waits: the runner's answer goes back over the control protocol —
// a refusal with the person's reason, and a question's answer as the person's message.
func TestSessionAsksTheApproverAndAnswers(t *testing.T) {
	b, argsFile, stdin := liveFake(t, "synthetic-session-permission.ndjson")
	ap := &recordingApprover{}
	rec := &replay.Recorder{}
	out := b.Run(context.Background(), backend.Spec{Dir: t.TempDir(), Prompt: "set it up", Live: &backend.Live{Approve: ap}}, rec.Emit)
	if out.Status != proto.StatusDone {
		t.Fatalf("outcome: %+v", out)
	}
	if len(ap.asked) != 2 || ap.asked[0].Tool != "Bash" || ap.asked[0].Kind != backend.RequestToolApproval ||
		!strings.Contains(string(ap.asked[0].Input), "curl") || ap.asked[1].Kind != backend.RequestQuestion {
		t.Fatalf("asked: %+v", ap.asked)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--permission-mode\nmanual\n--permission-prompts\nhost\n") || strings.Contains(string(args), "bypassPermissions") {
		t.Fatalf("a job with an approver is asked:\n%s", args)
	}
	written := mustJSON(readLines(t, stdin))
	for _, want := range []string{`"request_id":"perm-1"`, `"behavior":"deny"`, "not from the internet", `"request_id":"perm-2"`,
		"A person answered your question:\\n\\nPostgres"} {
		if !strings.Contains(written, want) {
			t.Fatalf("stdin lacks %s:\n%s", want, written)
		}
	}
	replay.Compare(t, filepath.Join("testdata", "golden", "synthetic-session-permission.golden.json"), rec.Events(), sessionOutcome(out))
}

// A read-only session is never asked anything: whatever would prompt is denied by the CLI itself,
// and a prompt that comes anyway is refused without a person — approvals never widen chat.
func TestReadOnlySessionNeverAsks(t *testing.T) {
	b, argsFile, stdin := liveFake(t, "synthetic-session-refused.ndjson")
	ap := &recordingApprover{}
	out := b.Run(context.Background(), backend.Spec{Dir: t.TempDir(), Prompt: "look", Access: backend.Access{ReadOnly: true},
		Live: &backend.Live{Approve: nil}}, func(string, any) {})
	if out.Status != proto.StatusDone || len(ap.asked) != 0 {
		t.Fatalf("outcome %+v, asked %+v", out, ap.asked)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--permission-mode\ndontAsk\n") || !strings.Contains(string(args), "--permission-prompts\nnone\n") {
		t.Fatalf("args:\n%s", args)
	}
	if written := mustJSON(readLines(t, stdin)); !strings.Contains(written, "Nobody can approve tools in this session") {
		t.Fatalf("a prompt was not refused: %s", written)
	}
}

// A steer that comes after the session closed its input is handed back, not lost.
func TestSessionHandsBackASteerItCouldNotDeliver(t *testing.T) {
	b, _, _ := liveFake(t, "synthetic-session-interrupt.ndjson")
	steer := make(chan string, 1)
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel(errors.New("stopped: by a person"))
		time.Sleep(300 * time.Millisecond)
		steer <- "too late"
	}()
	out := b.Run(ctx, backend.Spec{Dir: t.TempDir(), Prompt: "go", Live: &backend.Live{Steer: steer}}, func(string, any) {})
	if !out.Interrupted {
		t.Fatalf("outcome: %+v", out)
	}
	time.Sleep(500 * time.Millisecond) // the person speaks after the run ended
	select {
	case m := <-steer:
		if m != "too late" {
			t.Fatalf("steer: %q", m)
		}
	default:
		if len(out.Unread) != 1 || out.Unread[0] != "too late" {
			t.Fatalf("the steer was lost: %+v", out.Unread)
		}
	}
}

// A CLI that does not speak the session protocol is told apart from one that failed: the backend
// falls back to print mode for good and says so in its capabilities.
func TestSessionFallsBackToPrintModeOnAnOldCLI(t *testing.T) {
	b, _ := fake(t, "claude-2.1.270-tool-success.ndjson")
	dir := t.TempDir()
	old := filepath.Join(dir, "old-claude")
	wd, _ := os.Getwd()
	script := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = \"--input-format\" ]; then echo \"error: unknown option '--input-format'\" >&2; exit 1; fi; done\nexec " +
		filepath.Join(wd, "testdata", "fake-claude.sh") + " \"$@\"\n"
	if err := os.WriteFile(old, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b.Bin, b.Mode, b.Fallback = old, ModeSession, &atomic.Bool{}
	if !b.Capabilities().ActiveSteering {
		t.Fatal("a session backend steers")
	}
	var evs []string
	out := b.Run(context.Background(), backend.Spec{Dir: t.TempDir(), Prompt: "x", Live: &backend.Live{}}, func(typ string, _ any) { evs = append(evs, typ) })
	if out.Status != proto.StatusDone || out.Summary != "DONE" || evs[0] != "backend_fallback" {
		t.Fatalf("outcome %+v events %v", out, evs)
	}
	if c := b.Capabilities(); c.ActiveSteering || c.ToolApprovals {
		t.Fatalf("after the fallback: %+v", c)
	}
}

func sessionOutcome(o backend.Outcome) map[string]any {
	return map[string]any{"status": o.Status, "reason": o.Reason, "interrupted": o.Interrupted, "session_id": o.SessionID,
		"tool_calls": o.ToolCalls, "summary": o.Summary, "cost_usd": o.Usage.CostUSD}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
