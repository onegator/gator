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

// External references the plugin keeps on tasks.
const (
	refIssue    = "linear_issue"     // ENG-123
	refIssueID  = "linear_issue_id"  // the id the API wants
	refIssueURL = "linear_issue_url" //
)

const configSchema = `{"type":"object","properties":{
	"api_key":{"type":"string","x-secret":true,"description":"Linear personal API key; needed to move issues between states. Without it issues still become tasks, but Linear never hears what happened to them"},
	"webhook_secret":{"type":"string","x-secret":true,"description":"the signing secret of the Linear webhook"},
	"team":{"type":"string","description":"team key, like ENG; only its issues become tasks. Empty: every team the webhook sends"},
	"trigger_label":{"type":"string","description":"only issues with this label become tasks; empty: every new issue"},
	"state_map":{"type":"string","description":"which Linear state each phase moves the issue to, like implementation=In Progress, verification=In Review, approved=Done. Phases not listed leave the issue where it is"},
	"api_url":{"type":"string","description":"GraphQL endpoint; default https://api.linear.app/graphql"}
	},"required":["webhook_secret"]}`

var manifest = plugin.Manifest{
	Name:         "linear",
	Version:      "0.1.0",
	Capabilities: []string{"tracker"},
	UI:           []string{"tab", "chip"},
	ConfigSchema: json.RawMessage(configSchema),
	Webhook:      &plugin.WebhookSpec{DeliveryHeader: "Linear-Delivery"},
}

type linear struct{ http *http.Client }

func (l *linear) handlers() plugin.Handlers {
	return plugin.Handlers{Manifest: manifest, Webhook: l.webhook, PhaseTransition: l.phaseTransition, RenderUI: l.renderUI}
}

func setting(core *plugin.Core, key, def string) string {
	if v := core.Setting(key); v != "" {
		return v
	}
	return def
}

func header(h map[string]string, name string) string { return h[http.CanonicalHeaderKey(name)] }

// validSignature checks Linear-Signature, a hex HMAC-SHA256 of the body, in constant time.
func validSignature(secret string, body []byte, got string) bool {
	if secret == "" || got == "" {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal([]byte(got), []byte(hex.EncodeToString(m.Sum(nil))))
}

// issue is the part of Linear's payload this plugin reads.
type issue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Team        struct {
		Key string `json:"key"`
	} `json:"team"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (is issue) has(label string) bool {
	for _, l := range is.Labels {
		if strings.EqualFold(l.Name, label) {
			return true
		}
	}
	return false
}

func (l *linear) webhook(ctx context.Context, core *plugin.Core, p plugin.WebhookParams) error {
	if !validSignature(core.Secret("webhook_secret"), p.Body, header(p.Headers, "Linear-Signature")) {
		return plugin.Errorf(plugin.CodeForbidden, "bad or missing Linear-Signature")
	}
	var env struct {
		Action string `json:"action"`
		Type   string `json:"type"`
		Data   issue  `json:"data"`
	}
	if err := json.Unmarshal(p.Body, &env); err != nil {
		return plugin.Errorf(plugin.CodeInvalidParams, "body: %v", err)
	}
	if env.Type != "Issue" || (env.Action != "create" && env.Action != "update") {
		return nil // comments, projects, cycles: not tasks
	}
	is := env.Data
	if is.ID == "" || is.Identifier == "" {
		return nil
	}
	if team := core.Setting("team"); team != "" && !strings.EqualFold(is.Team.Key, team) {
		return core.Log(ctx, "info", "ignoring an issue from another team", map[string]any{"issue": is.Identifier})
	}
	// An update only matters when it adds the trigger label; otherwise every edit of every
	// issue would be a candidate task.
	if t := core.Setting("trigger_label"); t != "" && !is.has(t) {
		return nil
	}
	if env.Action == "update" && core.Setting("trigger_label") == "" {
		return nil
	}
	// Deliveries repeat and labels are added more than once: one issue, one task.
	if t, err := core.FindTask(ctx, refIssue, is.Identifier); err != nil || t != nil {
		return err
	}
	kind := "feature"
	if is.has("bug") {
		kind = "bug"
	}
	_, err := core.CreateTask(ctx, plugin.TaskCreateParams{Kind: kind, Title: is.Title,
		Description:  strings.TrimSpace(is.Description + "\n\n" + is.URL),
		ExternalRefs: map[string]string{refIssue: is.Identifier, refIssueID: is.ID, refIssueURL: is.URL}})
	return err
}

// parseStateMap reads "implementation=In Progress, approved=Done" into phase → state name.
func parseStateMap(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		phase, state, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if phase, state = strings.TrimSpace(phase), strings.TrimSpace(state); phase != "" && state != "" {
			out[strings.ToLower(phase)] = state
		}
	}
	return out
}

// phaseTransition mirrors the task's phase in the issue's state, so people who live in Linear
// see where the work is without opening Gator. Gator owns the phase; Linear only mirrors it.
func (l *linear) phaseTransition(ctx context.Context, core *plugin.Core, p plugin.PhaseTransitionParams) error {
	issueID := p.Task.ExternalRefs[refIssueID]
	if issueID == "" {
		return nil // not a task that came from Linear
	}
	state, ok := parseStateMap(core.Setting("state_map"))[strings.ToLower(p.To)]
	if !ok {
		return nil
	}
	key := core.Secret("api_key")
	if key == "" {
		return core.Log(ctx, "info", "no api_key, so Linear does not hear about phase changes", map[string]any{"issue": p.Task.ExternalRefs[refIssue]})
	}
	c := l.client(core, key)
	stateID, err := c.stateID(ctx, issueID, state)
	if err != nil {
		return asPluginErr(err)
	}
	if stateID == "" {
		return plugin.Errorf(plugin.CodeInvalidParams, "the issue's team has no state called %q; check state_map", state)
	}
	return asPluginErr(c.moveIssue(ctx, issueID, stateID))
}

func (l *linear) renderUI(ctx context.Context, core *plugin.Core, p plugin.RenderUIParams) (plugin.RenderUIResult, error) {
	id, url := p.Task.ExternalRefs[refIssue], p.Task.ExternalRefs[refIssueURL]
	if id == "" {
		return plugin.RenderUIResult{}, nil
	}
	md := fmt.Sprintf("- Issue: [%s](%s)", id, url)
	if state, ok := parseStateMap(core.Setting("state_map"))[strings.ToLower(p.Task.Phase)]; ok {
		md += "\n- In Linear this phase shows as **" + state + "**"
	}
	return plugin.RenderUIResult{
		Tabs:  []plugin.Tab{{Title: "Linear", Markdown: md}},
		Chips: []plugin.Chip{{Text: id, URL: url}},
	}, nil
}

func (l *linear) client(core *plugin.Core, key string) *Client {
	return &Client{URL: setting(core, "api_url", "https://api.linear.app/graphql"), Key: key, HTTP: l.http}
}

// asPluginErr decides what counts against the plugin. A refused key stays broken until a person
// fixes it, so it is worth tripping the breaker and saying why.
func asPluginErr(err error) error {
	var ae *APIError
	if !errors.As(err, &ae) {
		return err
	}
	switch {
	case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden:
		return plugin.Errorf(plugin.CodeInternal, "Linear refused the API key (%v)", ae)
	case ae.Status < 500 && ae.Status != http.StatusTooManyRequests:
		return plugin.Errorf(plugin.CodeInvalidParams, "%v", ae)
	}
	return fmt.Errorf("linear: %w", err)
}
