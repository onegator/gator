//go:build unix

package claude

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/onegator/gator/internal/runner/agentenv"
	"github.com/onegator/gator/internal/runner/backend"
)

// proc is one claude process in a process group of its own: its stdout and stderr through pipes
// of our own, and its stdin in session mode.
type proc struct {
	cmd        *exec.Cmd
	pgid       int
	stdout     *os.File
	stdin      io.WriteCloser
	stderr     *tail
	stderrPipe *os.File
	stderrDone chan struct{}
	started    time.Time
}

// spawn starts claude with args in the job's directory, with the agent's environment: an
// allow-list, not a deny-list — the runner's environment holds its token and whatever its operator
// exported, and an agent that can run any command can print all of it.
func (b Backend) spawn(spec backend.Spec, args []string, withStdin bool) (*proc, error) {
	bin, args := agentenv.Command(b.bin(), args, spec.DenyRead)
	cmd := exec.Command(bin, args...)
	cmd.Dir = spec.Dir
	cmd.Env = backend.AgentEnv(spec, b.AllowEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p := &proc{cmd: cmd, stderr: &tail{max: 8 << 10}, stderrDone: make(chan struct{})}
	if withStdin {
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
		p.stdin = in
	}
	// Stdout goes through our own pipe: a background process that inherits it must not be able
	// to keep this run alive after claude itself exits.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("pipe: %w", err)
	}
	cmd.Stdout = pw
	// Stderr gets its own pipe too. If it were an io.Writer, Go would copy it through a pipe of
	// its own and Wait would block until every inheritor (a background tool) closed it.
	er, ew, err := os.Pipe()
	if err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("pipe: %w", err)
	}
	cmd.Stderr = ew
	go func() {
		defer close(p.stderrDone)
		_, _ = io.Copy(p.stderr, er)
	}()
	p.started = time.Now()
	if err := cmd.Start(); err != nil {
		pw.Close()
		ew.Close()
		pr.Close()
		er.Close()
		<-p.stderrDone
		return nil, fmt.Errorf("start %s: %v", b.bin(), err)
	}
	pw.Close()
	ew.Close()
	p.stdout, p.stderrPipe, p.pgid = pr, er, cmd.Process.Pid
	return p, nil
}

// lines hands each line of stdout to fn until it ends, and answers what stopped it early: a line
// over 32 MiB, a broken pipe.
func (p *proc) lines(fn func([]byte)) error {
	sc := bufio.NewScanner(p.stdout)
	sc.Buffer(make([]byte, 1<<20), 32<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	return sc.Err()
}

// terminate asks the group to end and kills it after grace, unless it exits first.
func (p *proc) terminate(exited <-chan struct{}, grace time.Duration) {
	_ = syscall.Kill(-p.pgid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(grace):
		_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
	}
}

// release ends what is left of the run once claude exited: anything left in its group
// (background tools) goes, which also lets go of the pipes, and the readers finish.
func (p *proc) release(parsed <-chan struct{}) {
	_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
	select {
	case <-parsed:
	case <-time.After(3 * time.Second):
		p.stdout.Close()
		<-parsed
	}
	select {
	case <-p.stderrDone:
	case <-time.After(time.Second):
		p.stderrPipe.Close()
		<-p.stderrDone
	}
	p.stdout.Close()
	p.stderrPipe.Close()
}
