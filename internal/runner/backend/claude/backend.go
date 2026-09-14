//go:build unix

package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
)

// Backend runs `claude -p` headless with stream-json output.
type Backend struct {
	Bin         string // default "claude"
	Model       string // optional --model
	GracePeriod time.Duration
	Log         *slog.Logger
}

var _ backend.Backend = Backend{}

// errToolLimit ends a run that exceeded its tool-call budget.
var errToolLimit = errors.New("tool call limit reached")

// Name implements backend.Backend.
func (b Backend) Name() string { return "claude" }

func (b Backend) bin() string {
	if b.Bin == "" {
		return "claude"
	}
	return b.Bin
}

// AuthState implements backend.Backend. On macOS the login lives in the Keychain, which
// cannot be inspected without prompting, so it reports "unknown" there.
func (b Backend) AuthState() string {
	if _, err := exec.LookPath(b.bin()); err != nil {
		return "missing"
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		return "ok"
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
			return "ok"
		}
	}
	if runtime.GOOS == "darwin" {
		return "unknown"
	}
	return "missing"
}

// Run implements backend.Backend.
func (b Backend) Run(ctx context.Context, spec backend.Spec, emit backend.Emit) backend.Outcome {
	prompt := spec.Prompt
	if strings.HasPrefix(prompt, "-") { // would be parsed as a flag
		prompt = "Task: " + prompt
	}
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions"}
	if spec.SessionID != "" {
		args = append(args, "--resume", spec.SessionID)
	}
	if m := firstNonEmpty(spec.Model, b.Model); m != "" {
		args = append(args, "--model", m)
	}

	cmd := exec.Command(b.bin(), args...)
	cmd.Dir = spec.Dir
	cmd.Env = cleanEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Stdout goes through our own pipe: a background process that inherits it must not be
	// able to keep this run alive after claude itself exits.
	pr, pw, err := os.Pipe()
	if err != nil {
		return failed("pipe: " + err.Error())
	}
	defer pr.Close()
	cmd.Stdout = pw
	// Stderr gets its own pipe too. If it were an io.Writer, Go would copy it through a pipe
	// of its own and Wait would block until every inheritor (a background tool) closed it.
	er, ew, err := os.Pipe()
	if err != nil {
		pw.Close()
		return failed("pipe: " + err.Error())
	}
	defer er.Close()
	cmd.Stderr = ew
	stderr := &tail{max: 8 << 10}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderr, er)
	}()

	started := time.Now()
	if err := cmd.Start(); err != nil {
		pw.Close()
		ew.Close()
		return failed(fmt.Sprintf("start %s: %v", b.bin(), err))
	}
	pw.Close()
	ew.Close()
	pgid := cmd.Process.Pid

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	p := &Parser{}
	parsed := make(chan struct{})
	go func() {
		defer close(parsed)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 32<<20)
		for sc.Scan() {
			_ = p.Line(sc.Bytes(), func(typ string, payload any) { emit(typ, payload) })
			if spec.MaxToolCalls > 0 && p.ToolCalls > spec.MaxToolCalls && runCtx.Err() == nil {
				cancel(errToolLimit)
			}
		}
	}()

	exited := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			grace := b.GracePeriod
			if grace == 0 {
				grace = 5 * time.Second
			}
			select {
			case <-exited:
			case <-time.After(grace):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		case <-exited:
		}
	}()

	waitErr := cmd.Wait()
	close(exited)
	// Anything left in the group (background tools) goes now, which also releases the pipe.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case <-parsed:
	case <-time.After(3 * time.Second):
		pr.Close()
		<-parsed
	}
	select {
	case <-stderrDone:
	case <-time.After(time.Second):
		er.Close()
		<-stderrDone
	}
	finished := time.Now()

	out := backend.Outcome{SessionID: p.SessionID, Model: p.Model, ToolCalls: p.ToolCalls}
	out.Usage = proto.Usage{Backend: "claude", Model: p.Model, DurationMS: finished.Sub(started).Milliseconds(),
		StartedAt: started, FinishedAt: finished, CostEstimated: true}
	if r := p.Result; r != nil {
		out.Usage.InputTokens, out.Usage.OutputTokens = r.Tokens.Input, r.Tokens.Output
		out.Usage.CacheReadTokens, out.Usage.CacheWriteTokens = r.Tokens.CacheRead, r.Tokens.CacheWrite
		out.Usage.CostUSD, out.Usage.CostEstimated = r.CostUSD, r.CostEstimated
		out.Summary = clip(r.Text, 2000)
		if r.SessionID != "" {
			out.SessionID = r.SessionID
		}
	}

	cause := context.Cause(runCtx)
	switch {
	case errors.Is(cause, errToolLimit):
		out.Status, out.Reason = proto.StatusFailed, fmt.Sprintf("tool call limit (%d) reached", spec.MaxToolCalls)
	case runCtx.Err() != nil:
		out.Interrupted, out.Cause = true, cause
		out.Reason = cause.Error()
	case p.Result == nil:
		out.Status = proto.StatusFailed
		out.Reason = fmt.Sprintf("claude ended without a result (%s)", exitText(waitErr))
		if t := strings.TrimSpace(stderr.String()); t != "" {
			out.Reason += ": " + clip(t, 500)
		}
		if p.BadLines > 0 {
			out.Reason += fmt.Sprintf("; %d unparseable lines", p.BadLines)
		}
	case p.Result.IsError:
		out.Status = proto.StatusFailed
		out.Reason = "claude " + p.Result.Subtype
		if p.Result.TerminalReason != "" {
			out.Reason += " (" + p.Result.TerminalReason + ")"
		}
		if len(p.Result.Errors) > 0 {
			out.Reason += ": " + clip(strings.Join(p.Result.Errors, "; "), 500)
		}
	default:
		out.Status = proto.StatusDone
	}
	return out
}

func failed(reason string) backend.Outcome {
	return backend.Outcome{Status: proto.StatusFailed, Reason: reason}
}

// cleanEnv drops variables that make Claude Code think it runs nested inside another session.
func cleanEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT":
			continue
		}
		out = append(out, kv)
	}
	return out
}

func exitText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("exit %d", ee.ExitCode())
	}
	if err != nil {
		return err.Error()
	}
	return "exit 0"
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
