//go:build unix

package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/agentenv"
	"github.com/onegator/gator/internal/runner/backend"
)

// Backend runs one ACP agent: acp:<name>, a process per run in the job's worktree.
type Backend struct {
	Agent   Agent
	Profile Profile
	// AllowEnv names variables the agent gets beyond agentenv's allow-list and its profile's
	// login variables; a trailing * matches a prefix. The runner's own GATOR_* never pass.
	AllowEnv    []string
	GracePeriod time.Duration
	Log         *slog.Logger
	// Getenv and Home answer AuthState; nil reads the runner's own.
	Getenv func(string) string
	Home   func() (string, error)
}

var (
	_ backend.Backend = Backend{}
	_ backend.Limited = Backend{}
)

// New is the backend of one configured agent, with its profile.
func New(a Agent, allowEnv []string, log *slog.Logger) Backend {
	return Backend{Agent: a, Profile: ProfileFor(a.Name), AllowEnv: allowEnv, Log: log}
}

// Name implements backend.Backend.
func (b Backend) Name() string { return b.Agent.BackendName() }

// AuthState implements backend.Backend: no_cli when the command is not on this runner, else what
// the agent's profile can tell of its login.
func (b Backend) AuthState() string {
	if len(b.Agent.Command) == 0 {
		return "no_cli"
	}
	if _, err := exec.LookPath(b.Agent.Command[0]); err != nil {
		return "no_cli"
	}
	getenv, home := b.Getenv, b.Home
	if getenv == nil {
		getenv = os.Getenv
	}
	if home == nil {
		home = os.UserHomeDir
	}
	h, _ := home()
	return b.Profile.authState(getenv, h)
}

// Supports implements backend.Limited. A read-only job is kept read-only by the permission
// answers (and the agent's read-only mode where it has one), on a worktree the executor makes
// read-only too. A job with no tools at all is not something an ACP agent can promise: it runs
// its own tools, and some of them it does not ask about.
func (Backend) Supports(a backend.Access) bool { return !a.NoTools }

// Run implements backend.Backend.
func (b Backend) Run(ctx context.Context, spec backend.Spec, emit backend.Emit) backend.Outcome {
	if len(b.Agent.Command) == 0 {
		return failed(b.Name() + " has no command")
	}
	grace := b.GracePeriod
	if grace == 0 {
		grace = 5 * time.Second
	}
	bin, args := agentenv.Command(b.Agent.Command[0], b.Agent.Command[1:], spec.DenyRead)
	cmd := exec.Command(bin, args...)
	cmd.Dir = spec.Dir
	// An allow-list, as for Claude: the agent's login variables pass, the runner's token never.
	cmd.Env = append(agentenv.Filter(os.Environ(), append(append([]string{}, b.AllowEnv...), b.Profile.Env...)), spec.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return failed("stdin: " + err.Error())
	}
	// Our own pipes for stdout and stderr: a background process that inherits them must not keep
	// this run alive after the agent exits (see the Claude backend).
	pr, pw, err := os.Pipe()
	if err != nil {
		return failed("pipe: " + err.Error())
	}
	defer pr.Close()
	er, ew, err := os.Pipe()
	if err != nil {
		pw.Close()
		return failed("pipe: " + err.Error())
	}
	defer er.Close()
	cmd.Stdout, cmd.Stderr = pw, ew
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
		return failed(fmt.Sprintf("start %s: %v", b.Agent.Command[0], err))
	}
	pw.Close()
	ew.Close()
	pgid := cmd.Process.Pid
	exited := make(chan struct{})
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait(); close(exited) }()

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	d := newDriver(b.Name(), b.Profile, spec, emit, func(err error) { cancel(err) })
	c := newConn(pr, stdin, d.handle)

	s, err := d.start(runCtx, c)
	var r promptResult
	if err == nil {
		r, err = d.prompt(runCtx, c, s.sessionID, grace)
	}
	finished := time.Now()

	// The turn is over: the agent's stdin closes, it gets the grace period to exit, then the
	// group goes, background tools with it.
	_ = stdin.Close()
	select {
	case <-exited:
	case <-time.After(grace):
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(grace):
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	<-exited
	waitErr := <-waited
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		pr.Close()
		<-c.done
	}
	select {
	case <-stderrDone:
	case <-time.After(time.Second):
		er.Close()
		<-stderrDone
	}

	if err != nil && runCtx.Err() == nil && errors.Is(err, errClosed) {
		err = fmt.Errorf("the agent exited (%s)%s", exitText(waitErr), stderrNote(stderr.String()))
	} else if err != nil && runCtx.Err() == nil {
		err = fmt.Errorf("%w%s", err, stderrNote(stderr.String()))
	}
	out := d.outcome(runCtx, s, r, err, started, finished)
	if n := c.badLines(); n > 0 && out.Status == proto.StatusFailed {
		out.Reason += fmt.Sprintf("; %d unparseable lines", n)
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

func stderrNote(s string) string {
	if s = strings.TrimSpace(s); s != "" {
		return ": " + clip(s, 500)
	}
	return ""
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
