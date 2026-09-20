package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

const clientSecret = "s3cret"

var built struct {
	once sync.Once
	path string
	err  error
}

func pluginBinary(t *testing.T) string {
	t.Helper()
	built.once.Do(func() {
		dir, err := os.MkdirTemp("", "gator-plugin-sentry")
		if err != nil {
			built.err = err
			return
		}
		built.path = filepath.Join(dir, "sentry")
		if out, err := exec.Command("go", "build", "-o", built.path, ".").CombinedOutput(); err != nil {
			built.err = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if built.err != nil {
		t.Fatal(built.err)
	}
	return built.path
}

// fakeSentry is the one endpoint this plugin calls.
type fakeSentry struct {
	*httptest.Server
	mu       sync.Mutex
	resolved []string
	status   int
}

func newFakeSentry(t *testing.T) *fakeSentry {
	f := &fakeSentry{status: 200}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /issues/{id}/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.status != 200 {
			w.WriteHeader(f.status)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": "no"})
			return
		}
		f.resolved = append(f.resolved, r.PathValue("id"))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSentry) resolvedIssues() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.resolved...)
}

// start runs the plugin against an in-memory core, configured like a real installation.
func start(t *testing.T, core *plugintest.Core, api string, settings map[string]string) *plugintest.Process {
	t.Helper()
	ctx := context.Background()
	set := map[string]any{"api_url": api}
	for k, v := range settings {
		set[k] = v
	}
	p, err := plugintest.Start(ctx, []string{pluginBinary(t)}, core, plugintest.Options{
		Config:  set,
		Secrets: map[string]string{"client_secret": clientSecret, "auth_token": "t0ken"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func sign(body []byte) string {
	m := hmac.New(sha256.New, []byte(clientSecret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func deliver(t *testing.T, p *plugintest.Process, resource string, payload any) error {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return p.Call(context.Background(), plugin.MethodWebhook, plugin.WebhookParams{
		Body: body,
		Headers: map[string]string{
			"Sentry-Hook-Resource":  resource,
			"Sentry-Hook-Signature": sign(body),
		},
	}, nil)
}

func issuePayload(action, id, level string) map[string]any {
	return map[string]any{"action": action, "data": map[string]any{"issue": map[string]any{
		"id": id, "title": "TypeError: undefined is not a function", "level": level,
		"web_url": "https://sentry.io/organizations/acme/issues/" + id + "/",
	}}}
}

// The whole point of the plugin: an alert becomes an incident, and a hundred more alerts about
// the same fault stay one incident.
func TestAnAlertStormIsOneIncident(t *testing.T) {
	api := newFakeSentry(t)
	core := plugintest.NewCore("")
	p := start(t, core, api.URL, nil)

	for range 3 {
		if err := deliver(t, p, "issue", issuePayload("created", "4001", "error")); err != nil {
			t.Fatal(err)
		}
	}
	incidents := core.Incidents()
	if len(incidents) != 1 {
		t.Fatalf("three alerts about one fault should be one incident, got %d", len(incidents))
	}
	inc := incidents["sentry:4001"]
	if inc.Count != 3 || inc.Severity != "high" || inc.TaskID == "" {
		t.Fatalf("incident = %+v", inc)
	}
	if inc.Title != "TypeError: undefined is not a function" {
		t.Errorf("title = %q", inc.Title)
	}
}

// An alert quieter than the floor is not worth waking someone for.
func TestAlertsBelowTheFloorAreIgnored(t *testing.T) {
	api := newFakeSentry(t)
	core := plugintest.NewCore("")
	p := start(t, core, api.URL, map[string]string{"min_level": "error"})

	if err := deliver(t, p, "issue", issuePayload("created", "4002", "warning")); err != nil {
		t.Fatal(err)
	}
	if len(core.Incidents()) != 0 {
		t.Fatal("a warning below min_level should not open an incident")
	}
	if err := deliver(t, p, "issue", issuePayload("created", "4003", "fatal")); err != nil {
		t.Fatal(err)
	}
	if core.Incidents()["sentry:4003"].Severity != "critical" {
		t.Fatalf("fatal should be critical: %+v", core.Incidents())
	}
}

// Both directions of "it is over": Sentry saying so, and the task finishing here.
func TestResolvingTravelsBothWays(t *testing.T) {
	api := newFakeSentry(t)
	core := plugintest.NewCore("")
	p := start(t, core, api.URL, nil)

	if err := deliver(t, p, "issue", issuePayload("created", "4004", "error")); err != nil {
		t.Fatal(err)
	}
	if err := deliver(t, p, "issue", issuePayload("resolved", "4004", "error")); err != nil {
		t.Fatal(err)
	}
	if !core.Incidents()["sentry:4004"].Closed {
		t.Fatal("resolving in Sentry should close the incident here")
	}

	if err := p.Call(context.Background(), plugin.MethodIncidentClosed,
		plugin.IncidentClosedParams{Fingerprint: "sentry:4004"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := api.resolvedIssues(); len(got) != 1 || got[0] != "4004" {
		t.Fatalf("finishing the task should resolve the issue in Sentry, resolved = %v", got)
	}
}

// A delivery nobody signed is not from Sentry.
func TestAnUnsignedDeliveryIsRefused(t *testing.T) {
	api := newFakeSentry(t)
	core := plugintest.NewCore("")
	p := start(t, core, api.URL, nil)

	body, _ := json.Marshal(issuePayload("created", "4005", "error"))
	err := p.Call(context.Background(), plugin.MethodWebhook, plugin.WebhookParams{
		Body:    body,
		Headers: map[string]string{"Sentry-Hook-Resource": "issue", "Sentry-Hook-Signature": "deadbeef"},
	}, nil)
	if err == nil {
		t.Fatal("a bad signature should be refused")
	}
	if len(core.Incidents()) != 0 {
		t.Fatal("nothing should be recorded from an unsigned delivery")
	}
}

// An event alert carries the release, which is what ties a fault to what was deployed.
func TestAnEventAlertBlamesItsRelease(t *testing.T) {
	api := newFakeSentry(t)
	core := plugintest.NewCore("")
	p := start(t, core, api.URL, map[string]string{"environment": "production"})

	payload := map[string]any{"action": "triggered", "data": map[string]any{"event": map[string]any{
		"issue_id": "4006", "title": "Timeout talking to the database", "level": "error",
		"environment": "production", "release": "2.3.1",
	}}}
	if err := deliver(t, p, "event_alert", payload); err != nil {
		t.Fatal(err)
	}
	inc := core.Incidents()["sentry:4006"]
	if inc.ReleaseVersion != "2.3.1" {
		t.Fatalf("incident should blame the release: %+v", inc)
	}

	// An alert from somewhere else is not this project's production.
	payload["data"].(map[string]any)["event"].(map[string]any)["environment"] = "staging"
	payload["data"].(map[string]any)["event"].(map[string]any)["issue_id"] = "4007"
	if err := deliver(t, p, "event_alert", payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := core.Incidents()["sentry:4007"]; ok {
		t.Fatal("an alert from another environment should be ignored")
	}
}
