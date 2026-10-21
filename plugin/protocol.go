// Package plugin is the contract between gator-server and its plugins, plus a small SDK for
// writing plugins in Go. A plugin is a separate process that speaks JSON-RPC 2.0 with the
// core over stdin and stdout, one JSON message per line; stderr is its log. Calls go both
// ways: the core invokes hooks, the plugin invokes core methods. See docs/plugins.md.
package plugin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ProtocolVersion is the plugin protocol this package speaks.
const ProtocolVersion = 1

// Core → plugin methods.
const (
	MethodInitialize       = "initialize"
	MethodShutdown         = "shutdown" // notification
	MethodWebhook          = "webhook"
	MethodPhaseTransition  = "phaseTransition"
	MethodGateEvaluate     = "gateEvaluate"
	MethodArtifactApproved = "artifactApproved"
	MethodRelease          = "release"        // M6
	MethodIncidentClosed   = "incidentClosed" // M6
	MethodSchedule         = "schedule"
	MethodRenderUI         = "renderUI"
	MethodJobPrepare       = "jobPrepare"
	MethodJobFinish        = "jobFinish"
	MethodQualityEvaluate  = "qualityEvaluate" // M8
	MethodTurnEnding       = "turnEnding"      // M9
	MethodImport           = "import"
	MethodTaskClosed       = "taskClosed"
)

// Hooks are the core → plugin methods a manifest may declare.
var Hooks = []string{MethodWebhook, MethodPhaseTransition, MethodGateEvaluate, MethodArtifactApproved,
	MethodRelease, MethodIncidentClosed, MethodSchedule, MethodRenderUI, MethodJobPrepare, MethodJobFinish,
	MethodQualityEvaluate, MethodTurnEnding, MethodImport, MethodTaskClosed}

// Plugin → core methods.
const (
	CoreTaskCreate     = "task.create"
	CoreTaskFind       = "task.find"
	CoreTaskUpdate     = "task.update"
	CoreTaskClose      = "task.close"
	CoreTaskReopen     = "task.reopen"
	CoreGateSetCheck   = "gate.setCheck"
	CoreArtifactPut    = "artifact.put"
	CoreIncidentUpsert = "incident.upsert" // M6
	CoreIncidentClose  = "incident.close"  // M6
	CoreReleaseRecord  = "release.record"  // M6
	CoreKVGet          = "kv.get"
	CoreKVPut          = "kv.put"
	CoreLog            = "log"
)

// Capabilities a plugin can declare; the core asks for capabilities, never for plugin names.
var Capabilities = []string{"tracker", "vcs", "ci", "monitoring", "deploy", "notify"}

// Manifest describes a plugin. The plugin returns it from initialize.
type Manifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Description is one sentence a person reads before switching the plugin on.
	Description    string          `json:"description,omitempty"`
	MinCoreVersion string          `json:"min_core_version,omitempty"`
	Capabilities   []string        `json:"capabilities"`
	Hooks          []string        `json:"hooks"`
	ConfigSchema   json.RawMessage `json:"config_schema,omitempty"`
	UI             []string        `json:"ui,omitempty"` // "tab", "chip"
	Schedule       []Schedule      `json:"schedule,omitempty"`
	Webhook        *WebhookSpec    `json:"webhook,omitempty"`
	// Connect lets a person sign in to the outside system instead of pasting a token.
	Connect *ConnectSpec `json:"connect,omitempty"`
}

// Schedule asks the core to call the schedule hook every Every (a Go duration, at least 1s).
type Schedule struct {
	Name  string `json:"name"`
	Every string `json:"every"`
}

// WebhookSpec tells the core how to deduplicate incoming webhooks.
type WebhookSpec struct {
	DeliveryHeader string `json:"delivery_header,omitempty"` // e.g. "X-GitHub-Delivery"
}

// ConnectSpec describes an OAuth 2.0 authorization-code flow the server runs for the plugin.
// The server owns the OAuth client (GATOR_<PROVIDER>_CLIENT_ID and _CLIENT_SECRET in its own
// environment, never the plugin's), sends the person to AuthorizeURL with state and PKCE, trades
// the code at TokenURL, and stores the access token as the project's secret setting Secret. A
// token that expires is refreshed by the server before it does, and the plugin restarts with
// the new one, so a plugin only ever reads core.Secret(Secret).
type ConnectSpec struct {
	// Provider names the OAuth client in the server's environment: GATOR_<PROVIDER>_CLIENT_ID.
	// Default: the plugin's name.
	Provider     string   `json:"provider,omitempty"`
	Title        string   `json:"title,omitempty"` // "GitHub", for "Connect GitHub"
	AuthorizeURL string   `json:"authorize_url"`
	TokenURL     string   `json:"token_url"`
	Scopes       []string `json:"scopes,omitempty"`
	// ScopeSeparator joins Scopes; default a space. Linear wants a comma.
	ScopeSeparator string `json:"scope_separator,omitempty"`
	// Secret is the secret setting that receives the access token.
	Secret string `json:"secret"`
	// WebhookSecret, when set, is a secret setting the server fills with a random value on the
	// first connection, so a plugin that registers its own webhook has something to sign with.
	WebhookSecret string `json:"webhook_secret,omitempty"`
	// Params are added to the authorization URL, such as Linear's prompt=consent.
	Params map[string]string `json:"params,omitempty"`
}

// ProviderName is the provider the server looks up in its environment.
func (c ConnectSpec) ProviderName(plugin string) string {
	if c.Provider != "" {
		return c.Provider
	}
	return plugin
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ValidName reports whether s can name a plugin.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Validate checks a manifest before the core accepts it.
func (m Manifest) Validate() error {
	if !nameRe.MatchString(m.Name) {
		return fmt.Errorf("manifest: name %q must be lowercase letters, digits and dashes", m.Name)
	}
	if m.Version == "" {
		return fmt.Errorf("manifest: version is required")
	}
	for _, h := range m.Hooks {
		if !contains(Hooks, h) {
			return fmt.Errorf("manifest: unknown hook %q", h)
		}
	}
	for _, c := range m.Capabilities {
		if !contains(Capabilities, c) {
			return fmt.Errorf("manifest: unknown capability %q", c)
		}
	}
	for _, s := range m.Schedule {
		d, err := time.ParseDuration(s.Every)
		if err != nil || d < time.Second || s.Name == "" {
			return fmt.Errorf("manifest: schedule %q needs a name and every >= 1s", s.Name)
		}
	}
	var schema ConfigSchema
	if len(m.ConfigSchema) > 0 {
		var err error
		if schema, err = ParseConfigSchema(m.ConfigSchema); err != nil {
			return err
		}
	}
	if c := m.Connect; c != nil {
		if !strings.HasPrefix(c.AuthorizeURL, "https://") || !strings.HasPrefix(c.TokenURL, "https://") {
			return fmt.Errorf("manifest: connect needs https authorize_url and token_url")
		}
		if !nameRe.MatchString(c.ProviderName(m.Name)) {
			return fmt.Errorf("manifest: connect provider %q must be lowercase letters, digits and dashes", c.Provider)
		}
		// The token and the webhook secret land in secret settings; a public one would put
		// them in the clear in the database and in every API answer.
		for _, k := range []string{c.Secret, c.WebhookSecret} {
			if k != "" && !schema.IsSecret(k) {
				return fmt.Errorf("manifest: connect stores into %q, which must be a secret setting", k)
			}
		}
		if c.Secret == "" {
			return fmt.Errorf("manifest: connect needs the secret setting that receives the token")
		}
	}
	return nil
}

// HasHook reports whether the plugin handles a hook.
func (m Manifest) HasHook(h string) bool { return contains(m.Hooks, h) }

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

// InitializeParams is the first call of every process. ProjectID is empty when the core
// only reads the manifest (at install time). Config holds the project's non-secret
// settings; secrets arrive as GATOR_SECRET_<NAME> environment variables.
type InitializeParams struct {
	CoreVersion     string         `json:"core_version"`
	ProtocolVersion int            `json:"protocol_version"`
	ProjectID       string         `json:"project_id,omitempty"`
	ProjectSlug     string         `json:"project_slug,omitempty"`
	Config          map[string]any `json:"config,omitempty"`
	// WebhookURL is where this project's deliveries for the plugin arrive from the internet,
	// when the server knows its public address (GATOR_HOOKS_URL). A plugin that can register its
	// own webhook uses it; empty means a person has to set the webhook up by hand.
	WebhookURL string `json:"webhook_url,omitempty"`
}

// TaskRef is a task as plugins see it.
type TaskRef struct {
	ID           string            `json:"id"`
	ProjectID    string            `json:"project_id"`
	Kind         string            `json:"kind"`
	Title        string            `json:"title"`
	Phase        string            `json:"phase"`
	ExternalRefs map[string]string `json:"external_refs,omitempty"`
	Closed       bool              `json:"closed,omitempty"`
}

// JobRef is a runner job as plugins see it.
type JobRef struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Phase   string `json:"phase"`
	Backend string `json:"backend"`
	Status  string `json:"status,omitempty"`
}

// WebhookParams carries one HTTP delivery to /hooks/<project>/<plugin>. The plugin verifies
// the signature itself. Authorization and Cookie headers are never forwarded.
type WebhookParams struct {
	DeliveryID string            `json:"delivery_id"`
	Headers    map[string]string `json:"headers"` // canonical header names
	Body       []byte            `json:"body"`
	// Query is the webhook URL's query string, first value per key. Some senders cannot sign a
	// delivery, and a token in the URL they are given is the only proof they can offer.
	Query map[string]string `json:"query,omitempty"`
}

// PhaseTransitionParams follows every advance or rollback.
type PhaseTransitionParams struct {
	Task     TaskRef `json:"task"`
	From     string  `json:"from"`
	To       string  `json:"to"`
	Rollback bool    `json:"rollback,omitempty"`
	Reason   string  `json:"reason,omitempty"`
}

// GateEvaluateParams asks for the plugin's checks on the task's current gate.
type GateEvaluateParams struct {
	Task  TaskRef `json:"task"`
	Phase string  `json:"phase"`
}

// GateEvaluateResult lists checks; the core stores them as "plugin:<name>" checks.
type GateEvaluateResult struct {
	Checks []Check `json:"checks"`
}

// Check is one verdict on a gate.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pass | fail | pending
	Detail string `json:"detail,omitempty"`
}

// ArtifactApprovedParams follows a human approval of a phase's documents.
type ArtifactApprovedParams struct {
	Task  TaskRef `json:"task"`
	Phase string  `json:"phase"`
}

// ScheduleParams names the manifest schedule that fired.
type ScheduleParams struct {
	Name string `json:"name"`
}

// RenderUIParams asks for the plugin's tab and chips on a task.
type RenderUIParams struct {
	Task TaskRef `json:"task"`
}

// RenderUIResult is Markdown tabs and small chips; nothing richer in v1.
type RenderUIResult struct {
	Tabs  []Tab  `json:"tabs,omitempty"`
	Chips []Chip `json:"chips,omitempty"`
}

// Tab is a Markdown tab in the task detail.
type Tab struct {
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
}

// Chip is a short label on the task card.
type Chip struct {
	Text  string `json:"text"`
	Color string `json:"color,omitempty"`
	URL   string `json:"url,omitempty"`
}

// JobPrepareParams comes before a job is leased to a runner.
type JobPrepareParams struct {
	Task TaskRef `json:"task"`
	Job  JobRef  `json:"job"`
}

// JobPrepareResult adds instructions to the job's context. Never secrets: runners see it.
type JobPrepareResult struct {
	Instructions string `json:"instructions,omitempty"`
}

// JobFinishParams follows a job's receipt.
type JobFinishParams struct {
	Task    TaskRef         `json:"task"`
	Job     JobRef          `json:"job"`
	Receipt json.RawMessage `json:"receipt"`
}

// TurnEndingParams asks whether an agent's turn may end. The core asks when the runner says
// the agent is done, before the receipt is written — the moment at which a plugin still knows
// something the gate will only find out about ten minutes later.
type TurnEndingParams struct {
	Task TaskRef `json:"task"`
	Job  JobRef  `json:"job"`
	// Turn counts this job's endings: 1 is the first, 2 the one after a first objection.
	Turn         int      `json:"turn"`
	Status       string   `json:"status"` // what the receipt would say: done | failed | stopped
	Summary      string   `json:"summary,omitempty"`
	Commits      []string `json:"commits,omitempty"`
	ChangedPaths []string `json:"changed_paths,omitempty"`
	// Digest is the agent's own account of the session, when it wrote one.
	Digest json.RawMessage `json:"digest,omitempty"`
}

// TurnEndingResult is the plugin's answer. An empty objection means "yes, it may end" — the
// same as not handling the hook at all. An objection reaches the agent as data, not as an
// instruction, and the core allows only a couple of them per job before the turn ends anyway.
type TurnEndingResult struct {
	Objection string `json:"objection,omitempty"`
}

// ReleaseParams asks a deploy plugin to ship a task's work. The plugin answers when the
// deployment is under way and calls release.record once it knows the version.
type ReleaseParams struct {
	Task TaskRef `json:"task"`
}

// IncidentClosedParams says an incident's task is finished, so the plugin can resolve it in
// whatever tool raised it.
type IncidentClosedParams struct {
	Task        TaskRef `json:"task"`
	Fingerprint string  `json:"fingerprint"`
}

// ReleaseRecordParams records what reached an environment. Version and environment identify a
// release: deploying the same version again updates it rather than making a second one.
type ReleaseRecordParams struct {
	Version     string `json:"version"`
	CommitSHA   string `json:"commit_sha,omitempty"`
	URL         string `json:"url,omitempty"`
	Environment string `json:"environment,omitempty"` // default "production"
	TaskID      string `json:"task_id,omitempty"`     // the task this release carries
	// ObserveMinutes is how long to watch before the task is called finished. Zero means the
	// project's default; a release with no window settles at once.
	ObserveMinutes int `json:"observe_minutes,omitempty"`
}

// ReleaseRecordResult identifies the stored release.
type ReleaseRecordResult struct {
	ReleaseID string `json:"release_id"`
}

// IncidentUpsertParams reports something wrong in production. The fingerprint is what makes
// one fault one incident however many times the monitoring tool repeats itself.
type IncidentUpsertParams struct {
	Fingerprint    string `json:"fingerprint"`
	Title          string `json:"title"`
	Severity       string `json:"severity,omitempty"` // critical | high | medium | low
	URL            string `json:"url,omitempty"`
	ExternalID     string `json:"external_id,omitempty"`
	ReleaseVersion string `json:"release_version,omitempty"` // blames a release by version
}

// IncidentUpsertResult says what the core did with it.
type IncidentUpsertResult struct {
	IncidentID string `json:"incident_id"`
	TaskID     string `json:"task_id,omitempty"`
	Created    bool   `json:"created"` // false when this was a repeat of a known fault
}

// IncidentClosedResult is empty; closing is idempotent.
type IncidentCloseParams struct {
	Fingerprint string `json:"fingerprint"`
}

// QualityEvaluateParams asks a plugin what it knows about each part of the project. The core
// asks on a schedule, not per event: a scorecard answers "how are we doing", which is a
// question with a date on it.
type QualityEvaluateParams struct {
	Components []string `json:"components"` // component keys, as the catalogue names them
}

// QualityEvaluateResult is what the plugin found. A rule it could not check answers "unknown"
// rather than "fail": an outage in a scanner says nothing about the code, and scoring it as a
// failure would file a task about someone else's downtime.
type QualityEvaluateResult struct {
	Checks []QualityCheck `json:"checks"`
}

// QualityCheck is one rule's answer about one component.
type QualityCheck struct {
	Component string `json:"component,omitempty"` // empty: about the project as a whole
	Rule      string `json:"rule"`
	Status    string `json:"status"` // pass | fail | unknown
	Detail    string `json:"detail,omitempty"`
	Weight    int    `json:"weight,omitempty"` // default 1
}

// TaskCreateParams creates a task in the plugin's project, in its template's first phase.
type TaskCreateParams struct {
	Kind         string            `json:"kind"`
	Title        string            `json:"title"`
	Description  string            `json:"description,omitempty"`
	Urgency      int               `json:"urgency,omitempty"`
	ExternalRefs map[string]string `json:"external_refs,omitempty"`
}

// TaskFindParams finds the newest task of the project with external_refs[Key] == Value.
type TaskFindParams struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// TaskUpdateParams merges external references into a task and, when given, replaces its title
// or description: an issue edited where it lives should read the same on the board.
type TaskUpdateParams struct {
	TaskID       string            `json:"task_id"`
	ExternalRefs map[string]string `json:"external_refs,omitempty"`
	Title        *string           `json:"title,omitempty"`
	Description  *string           `json:"description,omitempty"`
}

// TaskStateParams closes or reopens a task. Either is a no-op when the task is already there,
// because the outside system tells us about its own echo of our change too.
type TaskStateParams struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
}

// ImportParams asks a tracker plugin to bring in what already exists: the open issues a webhook
// will never mention, because they were opened before the plugin was switched on.
type ImportParams struct{}

// ImportResult says what an import did. It must be idempotent: the core runs it when the
// plugin is switched on and whenever a person asks, so an issue already on the board counts
// as Existing, not as a second task.
type ImportResult struct {
	Created  int `json:"created"`
	Existing int `json:"existing"`
	// Truncated is true when there was more than the plugin was willing to bring in at once.
	Truncated bool   `json:"truncated,omitempty"`
	Note      string `json:"note,omitempty"`
}

// TaskClosedParams says a task of the project finished or was closed, so a tracker can close
// the issue it came from. A plugin that closed the task itself hears about it too.
type TaskClosedParams struct {
	Task   TaskRef `json:"task"`
	Reason string  `json:"reason,omitempty"`
}

// SetCheckParams sets one check on the task's current gate.
type SetCheckParams struct {
	TaskID string `json:"task_id"`
	Check
}

// ArtifactPutParams stores a document or link on a task; Phase defaults to the current one.
type ArtifactPutParams struct {
	TaskID  string `json:"task_id"`
	Phase   string `json:"phase,omitempty"`
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	URL     string `json:"url,omitempty"`
}

// ArtifactPutResult is the stored version.
type ArtifactPutResult struct {
	Version int `json:"version"`
}

// KVGetParams reads the plugin's own key-value store for its project.
type KVGetParams struct {
	Key string `json:"key"`
}

// KVGetResult is the stored value, if any.
type KVGetResult struct {
	Found bool            `json:"found"`
	Value json.RawMessage `json:"value,omitempty"`
}

// KVPutParams writes the plugin's key-value store.
type KVPutParams struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// LogParams writes a line to the core's log, tagged with the plugin.
type LogParams struct {
	Level   string         `json:"level"` // debug | info | warn | error
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}
