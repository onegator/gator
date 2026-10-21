package e2e

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/onegator/gator/internal/cli"
	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/server/events"
)

// talkingExec says a few things, slowly enough that a follower sees them arrive live, and
// finishes.
type talkingExec struct{}

func (talkingExec) Run(ctx context.Context, job proto.Job, io client.JobIO) proto.Finish {
	io.Emit("text", map[string]string{"text": "looking at the task"})
	time.Sleep(200 * time.Millisecond)
	io.Emit("tool_call", map[string]any{"name": "Bash", "input": map[string]string{"command": "go test ./..."}})
	time.Sleep(200 * time.Millisecond)
	io.Emit("text", map[string]string{"text": "all green"})
	return proto.Finish{Status: proto.StatusDone, Summary: "done",
		Usage: proto.Usage{Backend: job.Backend, Model: "fake-1", InputTokens: 10, OutputTokens: 5, DurationMS: 100}}
}

// A person works from the terminal: starts a project and a task, runs it, and watches the
// log arrive until the job ends — over the real server, the real WebSocket and a real runner.
func TestAPersonRunsATaskFromTheTerminal(t *testing.T) {
	h := newHarness(t)
	// job.finished travels through the outbox, which the server's relay publishes.
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	go (&events.Relay{Pool: h.pool, Hub: h.hub, Interval: 50 * time.Millisecond, Log: slog.Default()}).Run(relayCtx)

	backend := uniqueBackend()
	_, stop := h.startRunner(backend, talkingExec{})
	defer stop()

	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	env := &cli.Env{
		Stdin: strings.NewReader(h.admin), Stdout: &stdout, Stderr: &stderr,
		Getenv: func(string) string { return "" }, ConfigDir: dir,
		Store: cli.FileStore{Path: filepath.Join(dir, "credentials.json")}, HTTP: http.DefaultClient,
		Hostname: "e2e", PollInterval: 50 * time.Millisecond,
	}
	gator := func(args ...string) string {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if code := cli.Run(ctx, env, args); code != 0 {
			t.Fatalf("gator %s exited %d: %s", strings.Join(args, " "), code, stderr.String())
		}
		return stdout.String()
	}

	gator("login", "--server", h.srv.URL, "--token", "-")
	slug := fmt.Sprintf("cli%d", time.Now().UnixNano())
	gator("project", "create", slug, "--name", "From the terminal")
	if out := gator("projects"); !strings.Contains(out, slug) || !strings.Contains(out, "admin") {
		t.Fatalf("projects:\n%s", out)
	}
	out := gator("task", "new", "--kind", "bug", "Cart forgets items")
	id := regexp.MustCompile(`[0-9a-f-]{36}`).FindString(out)
	if id == "" {
		t.Fatalf("task new:\n%s", out)
	}
	if out := gator("tasks"); !strings.Contains(out, id) || !strings.Contains(out, "Cart forgets items") {
		t.Fatalf("tasks:\n%s", out)
	}

	gator("task", "run", id, "--backend", backend)
	out = gator("task", "logs", id, "-f")
	want := "looking at the task\n→ Bash go test ./...\nall green\n"
	if out != want {
		t.Errorf("logs -f:\n%s\nwant:\n%s\nstderr: %s", out, want, stderr.String())
	}
	if strings.Contains(stderr.String(), "no live stream") {
		t.Errorf("followed by polling, not the WebSocket: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), " done") {
		t.Errorf("did not say the job ended: %s", stderr.String())
	}

	gator("logout")
	if code := cli.Run(context.Background(), env, []string{"whoami"}); code == 0 {
		t.Error("still signed in after logout")
	}
}
