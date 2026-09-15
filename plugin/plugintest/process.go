package plugintest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/onegator/gator/plugin"
)

// Options configure a plugin process.
type Options struct {
	ProjectSlug string
	Config      map[string]any    // non-secret settings, sent in initialize
	Secrets     map[string]string // passed as GATOR_SECRET_<NAME>, as the server does
	Stderr      io.Writer         // the plugin's log; default os.Stderr
	Timeout     time.Duration     // per call; default 30s
}

// Process is a running plugin connected to a Core.
type Process struct {
	Manifest plugin.Manifest
	Core     *Core

	conn    *plugin.Conn
	cmd     *exec.Cmd
	waited  chan error
	closeIn func() error
	timeout time.Duration
}

// Start runs command with the server's minimal environment and initializes it for core's
// project. The manifest must be valid.
func Start(ctx context.Context, command []string, core *Core, o Options) (*Process, error) {
	if len(command) == 0 {
		return nil, errors.New("no plugin command")
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir(), "LANG=C.UTF-8",
		"GATOR_PLUGIN_PROJECT=" + o.ProjectSlug}
	for k, v := range o.Secrets {
		cmd.Env = append(cmd.Env, plugin.SecretEnv(k)+"="+v)
	}
	cmd.Stderr = o.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{Core: core, cmd: cmd, waited: make(chan error, 1), closeIn: stdin.Close, timeout: o.Timeout}
	p.conn = plugin.NewConn(stdout, stdin, core.Handler())
	go func() {
		<-p.conn.Done()
		p.waited <- cmd.Wait()
	}()
	err = p.Call(ctx, plugin.MethodInitialize, plugin.InitializeParams{CoreVersion: "plugintest", ProtocolVersion: plugin.ProtocolVersion,
		ProjectID: core.ProjectID, ProjectSlug: o.ProjectSlug, Config: o.Config}, &p.Manifest)
	if err == nil {
		err = p.Manifest.Validate()
	}
	if err != nil {
		p.kill()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	return p, nil
}

// Call invokes a hook with the per-call timeout.
func (p *Process) Call(ctx context.Context, method string, params, out any) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	err := p.conn.Call(ctx, method, params, out)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s did not answer within %s", method, p.timeout)
	}
	return err
}

// Close asks the plugin to stop, then kills it after 3 seconds.
func (p *Process) Close() error {
	_ = p.conn.Notify(plugin.MethodShutdown, nil)
	_ = p.closeIn()
	select {
	case err := <-p.waited:
		return err
	case <-time.After(3 * time.Second):
		p.kill()
		return errors.New("plugin did not exit after shutdown; killed")
	}
}

func (p *Process) kill() {
	if p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	<-p.waited
}
