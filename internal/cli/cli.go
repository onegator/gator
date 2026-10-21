// Package cli is the person's command-line tool, gator: sign in with Google, look at projects
// and tasks, start a task, run it and watch its log. It speaks the same /api/v1 as Gator.app,
// with a user token of its own.
//
// The agent's tool, gator-cli, stays a separate binary on purpose. It speaks with a job's
// identity, answers only in JSON, and the runner looks for it by name beside itself; a person's
// tool that can read their stored sign-in has no business on an agent's PATH.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/onegator/gator/internal/version"
)

// Exit codes match gator-cli's, so a script can tell the cases apart the same way for both.
const (
	exitOK       = 0
	exitUserErr  = 1 // the request was wrong: a missing argument, a refused value
	exitNetwork  = 2 // the server could not be reached
	exitAuth     = 3 // not signed in, or the token has expired or been revoked
	exitOther    = 4 // anything else, including the server failing
	exitConflict = 5 // the work moved on: the task is closed, the gate is not satisfied
)

const usage = `gator — work with Gator from the terminal.

Sign in
  gator login [--server URL]          sign in with Google in the browser
  gator login --token TOKEN|-         use a token made elsewhere (- reads stdin)
  gator logout                        revoke this machine's token and forget it
  gator whoami                        who the stored token belongs to

Projects
  gator projects                      the projects you can see, with your role
  gator project create SLUG [--name NAME] [--repo owner/repo|URL] [--branch BRANCH]
  gator use [SLUG]                    the project the other commands default to

Tasks
  gator tasks [--project SLUG] [--mine] [--all]
  gator task show ID
  gator task new [--project SLUG] [--kind feature|bug|chore] TITLE [--description TEXT]
  gator task advance ID [--reason TEXT]
  gator task approve ID
  gator task run ID [--cloud] [--backend NAME] [--instruction TEXT] [-f]
  gator task logs ID [-f] [--job JOB_ID]

Every command takes --json for output a script can read, and --server URL.
GATOR_SERVER and GATOR_TOKEN override the stored server and token.
Exit codes: 0 ok, 1 your request, 2 network, 3 sign-in, 4 server, 5 the work moved on.
`

// Env is everything a command touches outside itself, so tests can stand in for the world.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	// ConfigDir holds config.json (and, without a keychain, credentials.json).
	ConfigDir string
	Store     Store
	HTTP      *http.Client
	// OpenBrowser shows the sign-in page. Failing is fine: the URL is printed as well.
	OpenBrowser func(url string) error
	Hostname    string
	// LoginTimeout bounds the wait for the browser to come back.
	LoginTimeout time.Duration
	// PollInterval paces log following when the WebSocket is not available.
	PollInterval time.Duration
}

// DefaultEnv is the real terminal, the real config directory and the real keychain.
func DefaultEnv() *Env {
	dir := ConfigDir(os.Getenv)
	host, _ := os.Hostname()
	return &Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv,
		ConfigDir: dir, Store: DefaultStore(dir), HTTP: &http.Client{Timeout: 30 * time.Second},
		OpenBrowser: openBrowser, Hostname: strings.TrimSuffix(host, ".local"),
		LoginTimeout: 5 * time.Minute, PollInterval: 2 * time.Second,
	}
}

func openBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "linux":
		return exec.Command("xdg-open", u).Start()
	}
	return errors.New("no browser opener on " + runtime.GOOS)
}

// Run executes one command line and returns the exit code.
func Run(ctx context.Context, env *Env, args []string) int {
	err := dispatch(ctx, env, args)
	if err == nil {
		return exitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(env.Stdout, usage)
		return exitOK
	}
	if errors.Is(err, context.Canceled) {
		return 130 // interrupted, as a shell reports Ctrl-C
	}
	code := exitOther
	var ce *codedError
	var ae *APIError
	switch {
	case errors.As(err, &ce):
		code = ce.code
	case errors.As(err, &ae):
		code = ae.exitCode()
	}
	fmt.Fprintln(env.Stderr, "gator: "+err.Error())
	return code
}

type codedError struct {
	code int
	msg  string
}

func (e *codedError) Error() string { return e.msg }

func coded(code int, format string, args ...any) error {
	return &codedError{code: code, msg: fmt.Sprintf(format, args...)}
}

func dispatch(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(env.Stdout, usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	if cmd == "--version" || cmd == "version" {
		fmt.Fprintf(env.Stdout, "gator %s (%s)\n", version.Version, version.Commit)
		return nil
	}
	if cmd == "task" || cmd == "project" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return coded(exitUserErr, "%s needs a subcommand; run gator help", cmd)
		}
		cmd, rest = cmd+" "+rest[0], rest[1:]
	}
	run, ok := commands[cmd]
	if !ok {
		return coded(exitUserErr, "unknown command %q; run gator help", cmd)
	}
	return run(ctx, env, rest)
}

type command func(ctx context.Context, env *Env, args []string) error

var commands map[string]command

func init() {
	commands = map[string]command{
		"login":          cmdLogin,
		"logout":         cmdLogout,
		"whoami":         cmdWhoami,
		"projects":       cmdProjects,
		"project create": cmdProjectCreate,
		"use":            cmdUse,
		"tasks":          cmdTasks,
		"task show":      cmdTaskShow,
		"task new":       cmdTaskNew,
		"task advance":   cmdTaskAdvance,
		"task approve":   cmdTaskApprove,
		"task run":       cmdTaskRun,
		"task logs":      cmdTaskLogs,
		"project list":   cmdProjects,
		"task list":      cmdTasks,
	}
}

// globals are the flags every command takes.
type globals struct {
	json   bool
	server string
}

func newFlags(name string) (*flag.FlagSet, *globals) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g := &globals{}
	fs.BoolVar(&g.json, "json", false, "print JSON")
	fs.StringVar(&g.server, "server", "", "the Gator server")
	return fs, g
}

// parse accepts flags before and after the positional arguments, so
// `gator task new "Fix login" --kind bug` works as a person would type it.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, coded(exitUserErr, "%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		if args[0] == "--" {
			return append(positional, args[1:]...), nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// oneArg is the single positional argument a command needs, named for the error.
func oneArg(positional []string, what string) (string, error) {
	switch len(positional) {
	case 1:
		return positional[0], nil
	case 0:
		return "", coded(exitUserErr, "which %s? give it as an argument", what)
	}
	return "", coded(exitUserErr, "expected one %s, got %d arguments: %s", what, len(positional), strings.Join(positional, " "))
}
