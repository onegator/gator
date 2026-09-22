package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/onegator/gator/plugin"
	"github.com/onegator/gator/plugin/plugintest"
)

const hookToken = "a-long-random-webhook-token"

var built struct {
	once sync.Once
	path string
	err  error
}

func pluginBinary(t *testing.T) string {
	t.Helper()
	built.once.Do(func() {
		dir, err := os.MkdirTemp("", "gator-plugin-honeybadger")
		if err != nil {
			built.err = err
			return
		}
		built.path = filepath.Join(dir, "honeybadger")
		if out, err := exec.Command("go", "build", "-o", built.path, ".").CombinedOutput(); err != nil {
			built.err = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if built.err != nil {
		t.Fatal(built.err)
	}
	return built.path
}

// fakeHoneybadger is the one endpoint the plugin calls, and who called it with what.
type fakeHoneybadger struct {
	*httptest.Server
	mu       sync.Mutex
	resolved []string
	auth     []string
}

func newFake(t *testing.T) *fakeHoneybadger {
	f := &fakeHoneybadger{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v2/projects/{project}/faults/{fault}", func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		f.mu.Lock()
		f.resolved = append(f.resolved, r.PathValue("project")+"/"+r.PathValue("fault"))
		f.auth = append(f.auth, user)
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func start(t *testing.T, core *plugintest.Core, api string, settings map[string]any) *plugintest.Process {
	t.Helper()
	cfg := map[string]any{"api_url": api, "project_id": "77"}
	for k, v := range settings {
		cfg[k] = v
	}
	p, err := plugintest.Start(context.Background(), []string{pluginBinary(t)}, core, plugintest.Options{
		Config: cfg, Secrets: map[string]string{"webhook_token": hookToken, "auth_token": "hb-token"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func deliver(p *plugintest.Process, token string, payload any) error {
	body, _ := json.Marshal(payload)
	return p.Call(context.Background(), plugin.MethodWebhook, plugin.WebhookParams{
		Body: body, Query: map[string]string{"token": token}}, nil)
}

func faultPayload(event string, id int64, env string) map[string]any {
	return map[string]any{"event": event, "fault": map[string]any{
		"id": id, "project_id": 77, "klass": "NoMethodError", "message": "undefined method `name' for nil",
		"environment": env, "url": fmt.Sprintf("https://app.honeybadger.io/projects/77/faults/%d", id),
	}}
}

// Every notice of one fault is one incident, however many Honeybadger sends.
func TestAFaultIsOneIncident(t *testing.T) {
	core := plugintest.NewCore("")
	p := start(t, core, newFake(t).URL, nil)
	for range 3 {
		if err := deliver(p, hookToken, faultPayload("occurred", 501, "production")); err != nil {
			t.Fatal(err)
		}
	}
	incidents := core.Incidents()
	inc := incidents["honeybadger:501"]
	if len(incidents) != 1 || inc.Count != 3 || inc.Severity != "high" {
		t.Fatalf("incidents = %+v", incidents)
	}
	if inc.Title != "NoMethodError: undefined method `name' for nil" {
		t.Errorf("title = %q", inc.Title)
	}
}

// Honeybadger cannot sign a delivery, so the token in the URL is the only proof it came from
// there. Without it, anybody who learns the webhook address can open incidents.
func TestADeliveryWithoutTheTokenIsRefused(t *testing.T) {
	core := plugintest.NewCore("")
	p := start(t, core, newFake(t).URL, nil)
	for _, token := range []string{"", "guess"} {
		if err := deliver(p, token, faultPayload("occurred", 502, "production")); err == nil {
			t.Fatalf("token %q was accepted", token)
		}
	}
	if len(core.Incidents()) != 0 {
		t.Fatal("nothing should be recorded from an unauthenticated delivery")
	}
}

func TestResolvingTravelsBothWays(t *testing.T) {
	api := newFake(t)
	core := plugintest.NewCore("")
	p := start(t, core, api.URL, nil)
	if err := deliver(p, hookToken, faultPayload("occurred", 503, "production")); err != nil {
		t.Fatal(err)
	}
	if err := deliver(p, hookToken, faultPayload("resolved", 503, "production")); err != nil {
		t.Fatal(err)
	}
	if !core.Incidents()["honeybadger:503"].Closed {
		t.Fatal("resolving in Honeybadger should close the incident here")
	}
	if err := p.Call(context.Background(), plugin.MethodIncidentClosed,
		plugin.IncidentClosedParams{Fingerprint: "honeybadger:503"}, nil); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.resolved) != 1 || api.resolved[0] != "77/503" || api.auth[0] != "hb-token" {
		t.Fatalf("finishing the task should resolve the fault there: %v %v", api.resolved, api.auth)
	}
}

func TestFaultsFromAnotherEnvironmentAreIgnored(t *testing.T) {
	core := plugintest.NewCore("")
	p := start(t, core, newFake(t).URL, map[string]any{"environment": "production"})
	if err := deliver(p, hookToken, faultPayload("occurred", 504, "staging")); err != nil {
		t.Fatal(err)
	}
	if len(core.Incidents()) != 0 {
		t.Fatal("a staging fault is not this project's production")
	}
}
