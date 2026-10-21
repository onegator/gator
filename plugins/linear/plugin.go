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
	"time"

	"github.com/onegator/gator/plugin"
)

// External references the plugin keeps on tasks.
const (
	refIssue    = "linear_issue"     // ENG-123
	refIssueID  = "linear_issue_id"  // the id the API wants
	refIssueURL = "linear_issue_url" //
)

const configSchema = `{"type":"object","properties":{
	"api_key":{"type":"string","x-secret":true,"title":"API key","description":"set by Connect Linear; or a personal API key. Without one issues still become tasks from the webhook, but nothing is imported and Linear never hears what happened to them"},
	"webhook_secret":{"type":"string","x-secret":true,"title":"Webhook secret","description":"set by Connect Linear; or the signing secret of a webhook you made by hand"},
	"team":{"type":"string","title":"Team","description":"team key, like ENG; only its issues become tasks. Empty: every team"},
	"trigger_label":{"type":"string","title":"Only issues labelled","description":"only issues with this label become tasks; empty: every issue"},
	"state_map":{"type":"string","x-format":"pairs","title":"Linear state for each phase","description":"which Linear state each phase moves the issue to, like implementation=In Progress, verification=In Review. Phases not listed leave the issue where it is. closed names the state a finished task's issue goes to; default: the team's first completed state"},
	"api_url":{"type":"string","title":"API address","description":"GraphQL endpoint; default https://api.linear.app/graphql"}
	}}`

var manifest = plugin.Manifest{
	Name:    "linear",
	Version: "0.2.0",
	Description: "Issues become tasks and follow them both ways: the task's phase shows as the issue's state, " +
		"and an issue finished or reopened in Linear is finished or reopened here.",
	Capabilities: []string{"tracker"},
	UI:           []string{"tab", "chip"},
	ConfigSchema: json.RawMessage(configSchema),
	Webhook:      &plugin.WebhookSpec{DeliveryHeader: "Linear-Delivery"},
	// admin is what lets the plugin register its own webhook; a server whose people are not
	// Linear admins can ask for read,write with GATOR_LINEAR_SCOPES and add the webhook by hand.
	Connect: &plugin.ConnectSpec{Title: "Linear", AuthorizeURL: "https://linear.app/oauth/authorize",
		TokenURL: "https://api.linear.app/oauth/token", Scopes: []string{"read", "write", "admin"}, ScopeSeparator: ",",
		Secret: "api_key", WebhookSecret: "webhook_secret", Params: map[string]string{"prompt": "consent"}},
}

// maxImport bounds one import; see the GitHub plugin.
const maxImport = 500

type linear struct{ http *http.Client }

func (l *linear) handlers() plugin.Handlers {
	return plugin.Handlers{Manifest: manifest, Init: l.init, Webhook: l.webhook, PhaseTransition: l.phaseTransition,
		RenderUI: l.renderUI, Import: l.importIssues, TaskClosed: l.taskClosed}
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
	State State `json:"state"`
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
	t, err := core.FindTask(ctx, refIssue, is.Identifier)
	if err != nil {
		return err
	}
	if t != nil {
		return l.follow(ctx, core, *t, is)
	}
	// An update only matters when it adds the trigger label; otherwise every edit of every
	// issue would be a candidate task.
	if env.Action == "update" && core.Setting("trigger_label") == "" {
		return nil
	}
	_, _, err = l.ensureTask(ctx, core, is)
	return err
}

// ensureTask makes one task per issue: deliveries repeat, labels are added more than once, and
// an import may meet an issue a webhook already brought. A finished issue is not work.
func (l *linear) ensureTask(ctx context.Context, core *plugin.Core, is issue) (*plugin.TaskRef, bool, error) {
	if t := core.Setting("trigger_label"); t != "" && !is.has(t) {
		return nil, false, nil
	}
	if t, err := core.FindTask(ctx, refIssue, is.Identifier); err != nil || t != nil {
		return t, false, err
	}
	if is.State.finished() {
		return nil, false, nil
	}
	kind := "feature"
	if is.has("bug") {
		kind = "bug"
	}
	t, err := core.CreateTask(ctx, plugin.TaskCreateParams{Kind: kind, Title: is.Title, Description: issueDescription(is),
		ExternalRefs: map[string]string{refIssue: is.Identifier, refIssueID: is.ID, refIssueURL: is.URL}})
	if err != nil {
		return nil, false, err
	}
	return &t, true, nil
}

func issueDescription(is issue) string { return strings.TrimSpace(is.Description + "\n\n" + is.URL) }

// follow brings a task in line with its issue: the title and description people edit in
// Linear, and whether it is finished. The phase stays Gator's: a state in between says little
// about which phase the work is in, and guessing would move a task past its gates.
func (l *linear) follow(ctx context.Context, core *plugin.Core, t plugin.TaskRef, is issue) error {
	if is.Title != "" {
		desc := issueDescription(is)
		if _, err := core.UpdateTask(ctx, plugin.TaskUpdateParams{TaskID: t.ID, Title: &is.Title, Description: &desc}); err != nil {
			return err
		}
	}
	if is.State.Type == "" {
		return nil
	}
	// The state Gator itself set for the task's phase comes back as an update too; that echo
	// is not Linear deciding anything.
	if mapped, ok := parseStateMap(core.Setting("state_map"))[strings.ToLower(t.Phase)]; ok && strings.EqualFold(mapped, is.State.Name) && !t.Closed {
		return nil
	}
	switch {
	case is.State.finished() && !t.Closed:
		_, err := core.CloseTask(ctx, t.ID, "moved to "+is.State.Name+" in Linear")
		return err
	case !is.State.finished() && t.Closed:
		_, err := core.ReopenTask(ctx, t.ID, "moved back to "+is.State.Name+" in Linear")
		return err
	}
	return nil
}

func (a apiIssue) issue() issue {
	is := issue{ID: a.ID, Identifier: a.Identifier, Title: a.Title, Description: a.Description, URL: a.URL, State: a.State}
	is.Team.Key = a.Team.Key
	for _, n := range a.Labels.Nodes {
		is.Labels = append(is.Labels, struct {
			Name string `json:"name"`
		}{n.Name})
	}
	return is
}

// importIssues brings in the issues that are not finished, of the team and with the label
// when those are set. Issues already on the board are counted, not repeated.
func (l *linear) importIssues(ctx context.Context, core *plugin.Core, _ plugin.ImportParams) (plugin.ImportResult, error) {
	var out plugin.ImportResult
	key := core.Secret("api_key")
	if key == "" {
		return out, plugin.Errorf(plugin.CodeInvalidParams, "connect Linear first")
	}
	c := l.client(core, key)
	after := ""
	for {
		page, next, err := c.openIssues(ctx, core.Setting("team"), core.Setting("trigger_label"), after)
		if err != nil {
			return out, asPluginErr(err)
		}
		for _, a := range page {
			if out.Created+out.Existing >= maxImport {
				out.Truncated = true
				out.Note = fmt.Sprintf("brought in the newest %d open issues; set team or trigger_label to choose which", maxImport)
				return out, nil
			}
			t, made, err := l.ensureTask(ctx, core, a.issue())
			switch {
			case err != nil:
				return out, err
			case t == nil:
				// Not labelled, or finished: not work, and not on the board either.
			case made:
				out.Created++
			default:
				out.Existing++
			}
		}
		if next == "" {
			return out, nil
		}
		after = next
	}
}

// taskClosed moves a finished task's issue to the state named closed in state_map, or else to
// the team's first completed state. An issue that is already finished is left where it is,
// which is also what a task closed from Linear's side finds.
func (l *linear) taskClosed(ctx context.Context, core *plugin.Core, p plugin.TaskClosedParams) error {
	issueID, key := p.Task.ExternalRefs[refIssueID], core.Secret("api_key")
	if issueID == "" || key == "" {
		return nil
	}
	c := l.client(core, key)
	cur, states, err := c.issueStates(ctx, issueID)
	if err != nil {
		return asPluginErr(err)
	}
	if cur.finished() {
		return nil
	}
	want, named := parseStateMap(core.Setting("state_map"))["closed"]
	var target *State
	for i, s := range states {
		switch {
		case named && strings.EqualFold(s.Name, want):
			target = &states[i]
		case !named && s.Type == "completed" && (target == nil || s.Position < target.Position):
			target = &states[i]
		}
	}
	if target == nil {
		return plugin.Errorf(plugin.CodeInvalidParams, "the issue's team has no state to close it in; check state_map")
	}
	return asPluginErr(c.moveIssue(ctx, issueID, target.ID))
}

// init registers the Linear webhook when the server knows where deliveries arrive. Like the
// GitHub plugin it mends a deleted one on every start and never fails the start.
func (l *linear) init(_ context.Context, core *plugin.Core) error {
	hookURL, secret, key := core.WebhookURL(), core.Secret("webhook_secret"), core.Secret("api_key")
	if hookURL == "" || secret == "" || key == "" {
		return nil
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := l.ensureWebhook(ctx, l.client(core, key), hookURL, secret); err != nil {
			_ = core.Log(ctx, "warn", "could not set up the Linear webhook; add it by hand, or connect as a Linear admin",
				map[string]any{"url": hookURL, "err": err.Error()})
		}
	}()
	return nil
}

func (l *linear) ensureWebhook(ctx context.Context, c *Client, hookURL, secret string) error {
	hooks, err := c.webhooks(ctx)
	if err != nil {
		return err
	}
	for _, h := range hooks {
		if h.URL == hookURL {
			return c.updateWebhook(ctx, h.ID, hookURL, secret)
		}
	}
	return c.createWebhook(ctx, hookURL, secret)
}

// parseStateMap reads "implementation=In Progress, approved=Done" into phase → state name.
func parseStateMap(s string) map[string]string {
	out := map[string]string{}
	for pair := range strings.SplitSeq(s, ",") {
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
