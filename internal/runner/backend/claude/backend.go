//go:build unix

package claude

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
)

// Backend runs `claude -p` headless with stream-json output.
type Backend struct {
	Bin   string // default "claude"
	Model string // optional --model
	// MCPConfig is the only MCP configuration a job session may use (a JSON string or file
	// path for --mcp-config). Sessions always run with --strict-mcp-config, so servers and
	// claude.ai connectors attached to the logged-in account are not available to jobs.
	// Empty means no MCP servers at all.
	MCPConfig string
	// MCPDir is where a job's MCP configuration is written while it runs; empty: DefaultMCPDir.
	MCPDir      string
	GracePeriod time.Duration
	Log         *slog.Logger
	// AllowEnv names variables the agent gets beyond agentenv's allow-list; a trailing *
	// matches a prefix. The runner's own GATOR_* never pass.
	AllowEnv []string
	// Mode is ModePrint (the zero value) or ModeSession.
	Mode string
	// Fallback, when set, is shared by every copy of the backend: a CLI that refuses the session
	// protocol sets it, and from then on runs are print-mode and the capabilities say so.
	Fallback *atomic.Bool
}

// noMCP is the default: a job session gets no MCP servers.
const noMCP = `{"mcpServers":{}}`

var _ backend.Backend = Backend{}

// maxSummary bounds the final answer carried in the receipt, far below the 4 MiB message
// limit of the runner connection.
const maxSummary = 200_000

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

// Capabilities implements backend.Backend. In print mode every turn is its own process: a steer
// restarts it with --resume, a stop kills it, and nothing is asked before a tool. The runner still
// checkpoints its worktree.
func (b Backend) Capabilities() proto.BackendCapabilities {
	if b.live() {
		return sessionCapabilities
	}
	return proto.BackendCapabilities{Checkpoints: true, Enforcement: proto.EnforcementNative}
}

// keychainService is where Claude Code keeps a macOS login.
const keychainService = "Claude Code-credentials"

// AuthState implements backend.Backend.
//
// A command this runner cannot find is "no_cli", not "missing": launchd hands an agent a
// nearly empty PATH, so a Mac whose owner is signed in perfectly well can fail this check
// simply because claude sits in ~/.local/bin. Saying "missing" sent that person to log in
// again, which fixed nothing.
func (b Backend) AuthState() string {
	if _, err := exec.LookPath(b.bin()); err != nil {
		return "no_cli"
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
		return keychainState(b.keychainLookup)
	}
	return "missing"
}

// keychainLookup asks the login keychain whether the item exists, and nothing more. Reading a
// secret out of the keychain prompts the person; asking whether an entry is there returns only
// its attributes and does not, which is why this was worth a second look — "Gator cannot see
// your login" was a limit we assumed rather than one we met.
func (b Backend) keychainLookup() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", keychainService)
	cmd.Stdout, cmd.Stderr = nil, nil
	err := cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode(), nil
	}
	return 0, err
}

// keychainState reads that answer. Found is "ok" and errSecItemNotFound is "missing"; anything
// else — a locked keychain, no security command, a timeout — is "unknown", because not knowing
// is its own answer and dressing it as a verdict is how a person ends up logging in again on a
// machine that was signed in all along.
func keychainState(lookup func() (int, error)) string {
	code, err := lookup()
	switch {
	case err != nil:
		return "unknown"
	case code == 0:
		return "ok"
	case code == 44: // errSecItemNotFound
		return "missing"
	default:
		return "unknown"
	}
}

// Run implements backend.Backend: a print-mode run (one prompt, one process), or a session in
// ModeSession.
func (b Backend) Run(ctx context.Context, spec backend.Spec, emit backend.Emit) backend.Outcome {
	if b.live() {
		return b.runSession(ctx, spec, emit)
	}
	return b.runPrint(ctx, spec, emit)
}

// runPrint is `claude -p <prompt>`: the process ends with the turn, a steer restarts it with
// --resume, and a stop kills it.
func (b Backend) runPrint(ctx context.Context, spec backend.Spec, emit backend.Emit) backend.Outcome {
	prompt := spec.Prompt
	if strings.HasPrefix(prompt, "-") { // would be parsed as a flag
		prompt = "Task: " + prompt
	}
	common, cleanup, err := b.commonArgs(spec)
	if err != nil {
		return failed(err.Error())
	}
	defer cleanup()
	args := append([]string{"-p", prompt, "--output-format", "stream-json", "--verbose"}, common...)
	args = append(args, permissionArgs(spec.Access, mcpNames(spec.MCP), hasSkills(common))...)

	p, err := b.spawn(spec, args, false)
	if err != nil {
		return failed(err.Error())
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	parser := &Parser{}
	parsed := make(chan struct{})
	go func() {
		defer close(parsed)
		p.lines(func(line []byte) {
			_ = parser.Line(line, func(typ string, payload any) { emit(typ, payload) })
			if spec.MaxToolCalls > 0 && parser.ToolCalls > spec.MaxToolCalls && runCtx.Err() == nil {
				cancel(errToolLimit)
			}
		})
	}()

	exited := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			p.terminate(exited, b.grace())
		case <-exited:
		}
	}()

	waitErr := p.cmd.Wait()
	close(exited)
	p.release(parsed)
	return b.outcome(runCtx, spec, parser, p, waitErr)
}

// commonArgs are the flags every run has whatever its mode: Gator's and the runner's MCP servers
// (strictly those), the agent's skills, the session to resume, the model and the cost cap. The
// cleanup removes what they wrote (the job's MCP configuration, its skills).
func (b Backend) commonArgs(spec backend.Spec) ([]string, func(), error) {
	mcp, cleanMCP, err := mcpConfig(b.MCPConfig, b.MCPDir, spec.MCP)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp config: %w", err)
	}
	skills, cleanSkills, err := skillsDir(b.MCPDir, spec.Skills)
	if err != nil {
		cleanMCP()
		return nil, nil, fmt.Errorf("skills: %w", err)
	}
	cleanup := func() { cleanSkills(); cleanMCP() }
	args := []string{"--strict-mcp-config", "--mcp-config", mcp}
	if skills != "" {
		args = append(args, "--plugin-dir", skills)
	}
	if spec.SessionID != "" {
		args = append(args, "--resume", spec.SessionID)
	}
	if m := firstNonEmpty(spec.Model, b.Model); m != "" {
		args = append(args, "--model", m)
	}
	if spec.MaxCostUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", spec.MaxCostUSD))
	}
	return args, cleanup, nil
}

// hasSkills says whether the common flags load the agent's skills.
func hasSkills(args []string) bool { return slices.Contains(args, "--plugin-dir") }

func (b Backend) grace() time.Duration {
	if b.GracePeriod == 0 {
		return 5 * time.Second
	}
	return b.GracePeriod
}

// outcome is what a run came to, from what the parser read and how the process and the run's
// context ended.
func (b Backend) outcome(runCtx context.Context, spec backend.Spec, parser *Parser, p *proc, waitErr error) backend.Outcome {
	finished := time.Now()
	out := backend.Outcome{SessionID: parser.SessionID, Model: parser.Model, ToolCalls: parser.ToolCalls}
	out.Usage = proto.Usage{Backend: "claude", Model: parser.Model, DurationMS: finished.Sub(p.started).Milliseconds(),
		StartedAt: p.started, FinishedAt: finished, CostEstimated: true}
	if r := parser.Result; r != nil {
		out.Usage.InputTokens, out.Usage.OutputTokens = r.Tokens.Input, r.Tokens.Output
		out.Usage.CacheReadTokens, out.Usage.CacheWriteTokens = r.Tokens.CacheRead, r.Tokens.CacheWrite
		out.Usage.CostUSD, out.Usage.CostEstimated = r.CostUSD, r.CostEstimated
		// The final answer is the job's document (brief, plan, report, review), so it is kept
		// whole; the cap only guards the connection against a runaway answer.
		out.Summary = clip(r.Text, maxSummary)
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
	case parser.Result == nil:
		out.Status = proto.StatusFailed
		out.Reason = fmt.Sprintf("claude ended without a result (%s)", exitText(waitErr))
		if t := strings.TrimSpace(p.stderr.String()); t != "" {
			out.Reason += ": " + clip(t, 500)
		}
		if parser.BadLines > 0 {
			out.Reason += fmt.Sprintf("; %d unparseable lines", parser.BadLines)
		}
	case parser.Result.IsError:
		out.Status = proto.StatusFailed
		out.Reason = "claude " + parser.Result.Subtype
		if parser.Result.TerminalReason != "" {
			out.Reason += " (" + parser.Result.TerminalReason + ")"
		}
		if len(parser.Result.Errors) > 0 {
			out.Reason += ": " + clip(strings.Join(parser.Result.Errors, "; "), 500)
		}
	default:
		out.Status = proto.StatusDone
	}
	return out
}

func failed(reason string) backend.Outcome {
	return backend.Outcome{Status: proto.StatusFailed, Reason: reason}
}

func exitText(err error) string {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
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
