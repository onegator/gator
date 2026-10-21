// Command gator is a person's way to work with Gator from a terminal: sign in with Google,
// see projects and tasks, start a task, run it and follow its log. See internal/cli.
//
// It is not the agent's tool. gator-cli, which the runner puts beside every job, speaks with
// the job's identity and stays a binary of its own.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/onegator/gator/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, cli.DefaultEnv(), os.Args[1:])
	stop()
	os.Exit(code)
}
