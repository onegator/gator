package provision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/onegator/gator/internal/server/runners"
)

var spec = runners.MachineSpec{Name: "gator-job-abc", JobID: "job-1", ProjectID: "project-1", Backend: "claude",
	Env: map[string]string{"GATOR_RUNNER_TOKEN": "gtr_runner_secret", "CLAUDE_CODE_OAUTH_TOKEN": "oauth"}}

// fakeDocker is a docker CLI that records its arguments and the variables it was asked to pass.
func fakeDocker(t *testing.T, stopOutput string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	script := `#!/bin/sh
echo "ARGS $*" >> "` + log + `"
echo "TOKEN $GATOR_RUNNER_TOKEN" >> "` + log + `"
case "$1" in
run) echo "c0ffee" ;;
rm) if [ -n "` + stopOutput + `" ]; then echo "` + stopOutput + `" >&2; exit 1; fi ;;
esac
`
	bin = filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestDockerStartsAContainerWithoutPuttingSecretsOnTheCommandLine(t *testing.T) {
	bin, log := fakeDocker(t, "")
	d := &Docker{Bin: bin, Image: "gator-runner:test", Network: "gator", Env: []string{"PATH=/usr/bin:/bin"}}
	handle, err := d.Start(context.Background(), spec)
	if err != nil || handle != "c0ffee" {
		t.Fatalf("start: %q %v", handle, err)
	}
	b, _ := os.ReadFile(log)
	out := string(b)
	for _, want := range []string{"run --detach --rm --name gator-job-abc", "--label dev.onegator.gator.job=job-1", "--network gator",
		"--env CLAUDE_CODE_OAUTH_TOKEN --env GATOR_RUNNER_TOKEN gator-runner:test", "TOKEN gtr_runner_secret"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ARGS") && (strings.Contains(line, "gtr_runner_secret") || strings.Contains(line, "oauth")) {
			t.Fatalf("a secret is on the command line: %s", line)
		}
	}
	if err := d.Stop(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "ARGS rm --force c0ffee") {
		t.Fatalf("stop:\n%s", b)
	}
}

// --rm removes the container when its runner exits, so stopping one that is gone is success.
func TestDockerStopOfAGoneContainerIsNotAnError(t *testing.T) {
	bin, _ := fakeDocker(t, "Error response from daemon: No such container: c0ffee")
	d := &Docker{Bin: bin, Image: "x", Env: []string{}}
	if err := d.Stop(context.Background(), "c0ffee"); err != nil {
		t.Fatal(err)
	}
}

func TestFlyCreatesAMachineThatDestroysItselfAndDeletesIt(t *testing.T) {
	var mu sync.Mutex
	var created flyCreate
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer fly-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps/gator-runners/machines":
			_ = json.NewDecoder(r.Body).Decode(&created)
			_, _ = w.Write([]byte(`{"id":"148e","state":"created"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/apps/gator-runners/machines/148e":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete:
			http.Error(w, `{"error":"machine not found"}`, http.StatusNotFound)
		default:
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	defer srv.Close()

	f := &Fly{Token: "fly-token", App: "gator-runners", Image: "registry.fly.io/gator-runners:1", Region: "waw", BaseURL: srv.URL}
	id, err := f.Start(context.Background(), spec)
	if err != nil || id != "148e" {
		t.Fatalf("start: %q %v", id, err)
	}
	c := created.Config
	if created.Name != "gator-job-abc" || created.Region != "waw" || c.Image != f.Image || !c.AutoDestroy || c.Restart.Policy != "no" ||
		c.Env["GATOR_RUNNER_TOKEN"] != "gtr_runner_secret" || c.Metadata["gator_job"] != "job-1" || c.Guest.CPUs != 2 {
		t.Fatalf("created %+v", created)
	}
	if err := f.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(context.Background(), "gone"); err != nil {
		t.Fatalf("a machine that destroyed itself: %v", err)
	}
	if calls[1] != "DELETE /v1/apps/gator-runners/machines/148e?force=true" {
		t.Fatalf("calls %v", calls)
	}

	f.Token = "wrong"
	if _, err := f.Start(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a refused create: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }

	if s, err := FromEnv(get); err != nil || s.Provisioner != nil {
		t.Fatalf("unset: %+v %v", s, err)
	}
	env["GATOR_CLOUD_PROVISIONER"] = "docker"
	if _, err := FromEnv(get); err == nil {
		t.Fatal("docker without a server URL or image")
	}
	env["GATOR_CLOUD_SERVER_URL"] = "http://host.docker.internal:8080"
	env["GATOR_CLOUD_RUNNER_IMAGE"] = "gator-runner:dev"
	env["GATOR_CLOUD_RUNNER_ENV"] = "CLAUDE_CODE_OAUTH_TOKEN"
	if _, err := FromEnv(get); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("a named credential that is not set: %v", err)
	}
	env["CLAUDE_CODE_OAUTH_TOKEN"] = "oauth"
	s, err := FromEnv(get)
	if err != nil || s.Provisioner.Name() != "docker" || s.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "oauth" || len(s.Env) != 1 {
		t.Fatalf("docker: %+v %v", s, err)
	}
	env["GATOR_CLOUD_RUNNER_ENV"] = "GATOR_DATABASE_URL"
	env["GATOR_DATABASE_URL"] = "postgres://"
	if _, err := FromEnv(get); err == nil {
		t.Fatal("the server's own configuration may not be passed to machines")
	}
	env["GATOR_CLOUD_RUNNER_ENV"] = ""
	env["GATOR_CLOUD_PROVISIONER"] = "fly"
	if _, err := FromEnv(get); err == nil {
		t.Fatal("fly without its token and app")
	}
	env["FLY_API_TOKEN"], env["FLY_APP"] = "t", "a"
	if s, err := FromEnv(get); err != nil || s.Provisioner.Name() != "fly" {
		t.Fatalf("fly: %+v %v", s, err)
	}
	env["GATOR_CLOUD_PROVISIONER"] = "aws"
	if _, err := FromEnv(get); err == nil {
		t.Fatal("an unknown provisioner")
	}
}
