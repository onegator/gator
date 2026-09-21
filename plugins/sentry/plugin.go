package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/onegator/gator/plugin"
)

const configSchema = `{"type":"object","properties":{
	"auth_token":{"type":"string","x-secret":true,"description":"Sentry auth token with event:write, used to resolve an issue once its task is finished; leave empty to only receive alerts"},
	"client_secret":{"type":"string","x-secret":true,"description":"the client secret of the Sentry integration, which signs every webhook"},
	"api_url":{"type":"string","description":"API base; default https://sentry.io/api/0"},
	"min_level":{"type":"string","description":"lowest level that opens an incident: fatal, error, warning or info; default warning"},
	"environment":{"type":"string","description":"only alerts from this environment; empty: all of them"}
	},"required":["client_secret"]}`

var manifest = plugin.Manifest{
	Name:         "sentry",
	Version:      "0.1.0",
	Capabilities: []string{"monitoring"},
	ConfigSchema: json.RawMessage(configSchema),
	// Sentry names the delivery in this header, and the core refuses a repeat of one it has
	// already handled — an alert storm is one incident, not a hundred deliveries.
	Webhook: &plugin.WebhookSpec{DeliveryHeader: "Sentry-Hook-Signature"},
}

type sentry struct{ http *http.Client }

func (s *sentry) handlers() plugin.Handlers {
	return plugin.Handlers{Manifest: manifest, Webhook: s.webhook, IncidentClosed: s.incidentClosed}
}

func setting(core *plugin.Core, key, def string) string {
	if v := core.Setting(key); v != "" {
		return v
	}
	return def
}

func header(h map[string]string, name string) string { return h[http.CanonicalHeaderKey(name)] }

// validSignature checks Sentry-Hook-Signature, a hex HMAC-SHA256 of the body, in constant time.
func validSignature(secret string, body []byte, got string) bool {
	if secret == "" || got == "" {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal([]byte(got), []byte(hex.EncodeToString(m.Sum(nil))))
}

// levels are Sentry's, most serious first, mapped onto Gator's severities. An incident's
// severity is what decides where it lands in someone's inbox, so the mapping is the whole
// judgement this plugin makes.
var levels = []struct{ sentry, severity string }{
	{"fatal", "critical"},
	{"error", "high"},
	{"warning", "medium"},
	{"info", "low"},
	{"debug", "low"},
}

func severityFor(level string) string {
	for _, l := range levels {
		if l.sentry == strings.ToLower(level) {
			return l.severity
		}
	}
	return "medium"
}

// loudEnough says whether a level reaches the configured floor. Sentry sends debug and info
// alerts that nobody wants to be woken for, and an incident that is not worth a task is worse
// than none: it teaches people to ignore the inbox.
func loudEnough(level, min string) bool {
	rank := func(name string) int {
		for i, l := range levels {
			if l.sentry == strings.ToLower(name) {
				return i
			}
		}
		return len(levels)
	}
	return rank(level) <= rank(min)
}

// issue is the part of Sentry's payload this plugin reads.
type issue struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Culprit   string `json:"culprit"`
	Level     string `json:"level"`
	Status    string `json:"status"`
	WebURL    string `json:"web_url"`
	Permalink string `json:"permalink"`
	Project   struct {
		Slug string `json:"slug"`
	} `json:"project"`
	Metadata struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"metadata"`
}

func (i issue) url() string {
	if i.WebURL != "" {
		return i.WebURL
	}
	return i.Permalink
}

// fingerprint is Sentry's issue id: the same fault, however many events it produces, is one
// issue there and must stay one incident here.
func (i issue) fingerprint() string { return "sentry:" + i.ID }

func (i issue) title() string {
	if i.Title != "" {
		return i.Title
	}
	if i.Metadata.Type != "" {
		return strings.TrimSpace(i.Metadata.Type + " " + i.Metadata.Value)
	}
	return "Sentry issue " + i.ID
}

func (s *sentry) webhook(ctx context.Context, core *plugin.Core, p plugin.WebhookParams) error {
	if !validSignature(core.Secret("client_secret"), p.Body, header(p.Headers, "Sentry-Hook-Signature")) {
		return plugin.Errorf(plugin.CodeForbidden, "bad or missing Sentry-Hook-Signature")
	}
	resource := header(p.Headers, "Sentry-Hook-Resource")
	var env struct {
		Action string `json:"action"`
		Data   struct {
			Issue *issue `json:"issue"`
			Event *struct {
				issue
				IssueID     string `json:"issue_id"`
				Environment string `json:"environment"`
				Release     string `json:"release"`
			} `json:"event"`
		} `json:"data"`
	}
	if err := json.Unmarshal(p.Body, &env); err != nil {
		return plugin.Errorf(plugin.CodeInvalidParams, "body: %v", err)
	}
	switch resource {
	case "issue":
		if env.Data.Issue == nil {
			return nil
		}
		return s.onIssue(ctx, core, env.Action, *env.Data.Issue, "")
	case "event_alert", "error":
		if env.Data.Event == nil {
			return nil
		}
		ev := env.Data.Event
		if want := core.Setting("environment"); want != "" && ev.Environment != "" && ev.Environment != want {
			return core.Log(ctx, "info", "ignoring an alert from another environment",
				map[string]any{"environment": ev.Environment})
		}
		it := ev.issue
		if it.ID == "" {
			it.ID = ev.IssueID
		}
		return s.onIssue(ctx, core, "created", it, ev.Release)
	}
	return nil
}

// onIssue turns what Sentry says about an issue into what the core keeps: a fault that is
// happening, or one that has stopped.
func (s *sentry) onIssue(ctx context.Context, core *plugin.Core, action string, it issue, release string) error {
	if it.ID == "" {
		return nil
	}
	switch action {
	case "resolved", "ignored":
		// Somebody fixed or muted it in Sentry. The task it opened stays open — whether the
		// work is finished is a person's call — but production is quiet again.
		return core.CloseIncident(ctx, plugin.IncidentCloseParams{Fingerprint: it.fingerprint()})
	case "created", "unresolved", "reopened", "":
	default:
		return nil // assigned, commented and the rest are not our business
	}
	if !loudEnough(it.Level, setting(core, "min_level", "warning")) {
		return core.Log(ctx, "info", "alert below min_level", map[string]any{"level": it.Level, "issue": it.ID})
	}
	_, err := core.ReportIncident(ctx, plugin.IncidentUpsertParams{
		Fingerprint: it.fingerprint(), Title: it.title(), Severity: severityFor(it.Level),
		URL: it.url(), ExternalID: it.ID, ReleaseVersion: release,
	})
	return err
}

// incidentClosed resolves the issue in Sentry once its task is finished. Without this the
// alert stays open there and the next occurrence looks like an old fault nobody dealt with.
func (s *sentry) incidentClosed(ctx context.Context, core *plugin.Core, p plugin.IncidentClosedParams) error {
	id := strings.TrimPrefix(p.Fingerprint, "sentry:")
	if id == "" || id == p.Fingerprint {
		return nil // not ours
	}
	token := core.Secret("auth_token")
	if token == "" {
		return core.Log(ctx, "info", "no auth_token, leaving the issue open in Sentry", map[string]any{"issue": id})
	}
	err := s.client(core, token).resolve(ctx, id)
	return asPluginErr(err)
}

func (s *sentry) client(core *plugin.Core, token string) *Client {
	return &Client{Base: strings.TrimRight(setting(core, "api_url", "https://sentry.io/api/0"), "/"),
		Token: token, HTTP: s.http}
}

// asPluginErr decides what counts against the plugin. A refused token stays broken until a
// person fixes it, so it is worth tripping the breaker and saying why; a 404 is an issue
// somebody deleted, which is nobody's emergency.
func asPluginErr(err error) error {
	var ae *APIError
	if !errors.As(err, &ae) {
		return err
	}
	switch {
	case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden:
		return plugin.Errorf(plugin.CodeInternal, "Sentry refused the token (%v). It needs event:write to resolve an issue", ae)
	case ae.Status == http.StatusNotFound:
		return nil
	case ae.Status < 500 && ae.Status != http.StatusTooManyRequests:
		return plugin.Errorf(plugin.CodeInvalidParams, "%v", ae)
	}
	return fmt.Errorf("sentry: %w", err)
}
