package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/onegator/gator/plugin"
)

const configSchema = `{"type":"object","properties":{
	"webhook_token":{"type":"string","x-secret":true,"description":"a long random string; put it at the end of the webhook URL as ?token=… — Honeybadger does not sign deliveries, so this is the only proof one came from it"},
	"auth_token":{"type":"string","x-secret":true,"description":"personal auth token, used only to resolve a fault once its task is finished; leave empty to only receive faults"},
	"project_id":{"type":"string","description":"Honeybadger project id, needed to resolve faults"},
	"api_url":{"type":"string","description":"API base; default https://app.honeybadger.io"},
	"environment":{"type":"string","description":"only faults from this environment; empty: all of them"},
	"severity":{"type":"string","description":"how loudly a new fault asks: critical, high, medium or low; default high"}
	},"required":["webhook_token"]}`

var manifest = plugin.Manifest{
	Name:         "honeybadger",
	Version:      "0.1.0",
	Capabilities: []string{"monitoring"},
	ConfigSchema: json.RawMessage(configSchema),
}

type honeybadger struct{ http *http.Client }

func (hb *honeybadger) handlers() plugin.Handlers {
	return plugin.Handlers{Manifest: manifest, Webhook: hb.webhook, IncidentClosed: hb.incidentClosed}
}

func setting(core *plugin.Core, key, def string) string {
	if v := core.Setting(key); v != "" {
		return v
	}
	return def
}

// fault is the part of Honeybadger's payload this plugin reads.
type fault struct {
	ID           int64  `json:"id"`
	ProjectID    int64  `json:"project_id"`
	Klass        string `json:"klass"`
	Message      string `json:"message"`
	Environment  string `json:"environment"`
	URL          string `json:"url"`
	NoticesCount int    `json:"notices_count"`
}

// fingerprint is Honeybadger's fault id: every notice of one fault is one incident.
func (f fault) fingerprint() string { return "honeybadger:" + strconv.FormatInt(f.ID, 10) }

func (f fault) title() string {
	msg := strings.TrimSpace(f.Message)
	switch {
	case f.Klass != "" && msg != "":
		return f.Klass + ": " + msg
	case f.Klass != "":
		return f.Klass
	case msg != "":
		return msg
	}
	return "Honeybadger fault " + strconv.FormatInt(f.ID, 10)
}

func (hb *honeybadger) webhook(ctx context.Context, core *plugin.Core, p plugin.WebhookParams) error {
	want := core.Secret("webhook_token")
	if want == "" || subtle.ConstantTimeCompare([]byte(p.Query["token"]), []byte(want)) != 1 {
		return plugin.Errorf(plugin.CodeForbidden, "missing or wrong ?token= on the webhook URL")
	}
	var env struct {
		Event string `json:"event"`
		Fault *fault `json:"fault"`
	}
	if err := json.Unmarshal(p.Body, &env); err != nil {
		return plugin.Errorf(plugin.CodeInvalidParams, "body: %v", err)
	}
	if env.Fault == nil || env.Fault.ID == 0 {
		return nil // uptime checks, deploys and the rest are not faults
	}
	f := *env.Fault
	if want := core.Setting("environment"); want != "" && f.Environment != "" && f.Environment != want {
		return core.Log(ctx, "info", "ignoring a fault from another environment", map[string]any{"environment": f.Environment})
	}
	switch env.Event {
	case "resolved":
		// Somebody resolved it in Honeybadger. The task stays open — whether the fix is
		// finished is a person's call — but production is quiet again.
		return core.CloseIncident(ctx, plugin.IncidentCloseParams{Fingerprint: f.fingerprint()})
	case "occurred", "unresolved", "reopened", "rate_exceeded":
		_, err := core.ReportIncident(ctx, plugin.IncidentUpsertParams{
			Fingerprint: f.fingerprint(), Title: f.title(), Severity: setting(core, "severity", "high"),
			URL: f.URL, ExternalID: strconv.FormatInt(f.ID, 10)})
		return err
	}
	return nil // assigned, commented and the rest are not our business
}

// incidentClosed resolves the fault in Honeybadger once its task is finished.
func (hb *honeybadger) incidentClosed(ctx context.Context, core *plugin.Core, p plugin.IncidentClosedParams) error {
	id := strings.TrimPrefix(p.Fingerprint, "honeybadger:")
	if id == "" || id == p.Fingerprint {
		return nil // not ours
	}
	token, project := core.Secret("auth_token"), core.Setting("project_id")
	if token == "" || project == "" {
		return core.Log(ctx, "info", "no auth_token or project_id, leaving the fault open in Honeybadger", map[string]any{"fault": id})
	}
	c := &Client{Base: strings.TrimRight(setting(core, "api_url", "https://app.honeybadger.io"), "/"), Token: token, HTTP: hb.http}
	return asPluginErr(c.resolve(ctx, project, id))
}

// asPluginErr decides what counts against the plugin: a refused token stays broken until a
// person fixes it; a fault somebody deleted is nobody's emergency.
func asPluginErr(err error) error {
	var ae *APIError
	if !errors.As(err, &ae) {
		return err
	}
	switch {
	case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden:
		return plugin.Errorf(plugin.CodeInternal, "Honeybadger refused the auth token (%v)", ae)
	case ae.Status == http.StatusNotFound:
		return nil
	case ae.Status < 500 && ae.Status != http.StatusTooManyRequests:
		return plugin.Errorf(plugin.CodeInvalidParams, "%v", ae)
	}
	return fmt.Errorf("honeybadger: %w", err)
}
