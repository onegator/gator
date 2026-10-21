//go:build unix

package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
)

// shedToken takes GATOR_RUNNER_TOKEN out of this process's environment for good. The agent never
// inherits it — the backend filters what it passes on — but the environment a process was
// started with stays readable in /proc/<pid>/environ by every process of the same user, the
// agent included. So the runner re-executes itself without the variable and hands the token
// over through a pipe instead (GATOR_RUNNER_TOKEN_FD). After that, nothing of this process
// that another can read holds the token. A failure leaves things as they were and says so.
func shedToken() error {
	token := os.Getenv("GATOR_RUNNER_TOKEN")
	if token == "" || os.Getenv("GATOR_RUNNER_TOKEN_FD") != "" {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		return err
	}
	// The token is far smaller than a pipe's buffer, so it is written before anyone reads.
	if _, err := syscall.Write(p[1], []byte(token)); err != nil {
		syscall.Close(p[0])
		syscall.Close(p[1])
		return err
	}
	syscall.Close(p[1])
	// The read end must survive exec.
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(p[0]), syscall.F_SETFD, 0); errno != 0 {
		syscall.Close(p[0])
		return errno
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GATOR_RUNNER_TOKEN=") })
	env = append(env, fmt.Sprintf("GATOR_RUNNER_TOKEN_FD=%d", p[0]))
	err = syscall.Exec(self, os.Args, env)
	syscall.Close(p[0]) // only reached when exec failed
	return err
}
