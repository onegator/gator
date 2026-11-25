// Package proto is the only package shared between gator-server and gator-runner.
// It defines the wire contract of the runner WebSocket and is versioned independently
// of either binary: the server accepts protocol versions N and N-1.
package proto

import (
	"encoding/json"
	"fmt"
	"time"
)

// Version is the current runner protocol version. Bump on any incompatible change
// and keep the previous version readable on the server side.
//
// v2 added the turn_ending/continue exchange: a runner asks before it ends a turn and
// something may object. A v1 runner never asks, so it behaves exactly as it always did.
//
// v3 added agents' turns: a job says its Kind (a phase, a reply in a conversation, a summary),
// its Tier (chat is read-only), its Agent and, for a reply, the conversation it answers in. A v2
// runner knows none of it and would run a reply as a phase (committing, pushing, objecting to
// itself), so it is only ever handed phase jobs, which are all of the work tier.
//
// v4 added live sessions: a runner says what each of its backends can do (BackendCapabilities),
// sends a checkpoint of the worktree after every turn, asks a person before a tool the project
// wants asked about (runtime_request, answered by runtime_decision), and is handed the
// attachments of the conversation and the approval policy with a job. A v3 runner knows none of
// it and runs a job as it always did; the server no longer speaks v2.
const Version = 4

// MinSupportedVersion is the oldest runner protocol the server still speaks.
const MinSupportedVersion = 3

// Timing both sides agree on.
const (
	HeartbeatInterval = 15 * time.Second
	// OfflineAfter without a heartbeat the server marks the runner offline.
	OfflineAfter = 45 * time.Second
	// LeaseTTL is how long a lease lives without being extended by a heartbeat.
	LeaseTTL = 90 * time.Second
)

// Path is where the runner WebSocket is served, relative to the server base URL.
const Path = "/api/v1/runner"

// MessageType enumerates every message either side may send.
type MessageType string

// Runner → server.
const (
	TypeRegister     MessageType = "register"
	TypeHeartbeat    MessageType = "heartbeat"
	TypeLeaseRequest MessageType = "lease_request"
	TypeEvents       MessageType = "events"
	TypeFinish       MessageType = "finish"
	TypeLoginPrompt  MessageType = "login_prompt"
	TypeTurnEnding   MessageType = "turn_ending" // v2
	TypeCheckpoint   MessageType = "checkpoint"  // v4, kept until acked like events
	// TypeRuntimeRequest asks a person, mid-turn, before a tool (v4). Kept until acked, and sent
	// again after a reconnect: the server answers a request it already decided with its decision.
	TypeRuntimeRequest MessageType = "runtime_request"
)

// Server → runner.
const (
	TypeRegistered   MessageType = "registered"
	TypeLease        MessageType = "lease"
	TypeAck          MessageType = "ack"
	TypeSteer        MessageType = "steer"
	TypeStop         MessageType = "stop"
	TypeLoginBackend MessageType = "login_backend"
	TypeError        MessageType = "error"
	TypeContinue     MessageType = "continue" // v2, the answer to turn_ending
	// TypeRuntimeDecision answers a runtime_request (v4), once it is decided or expired.
	TypeRuntimeDecision MessageType = "runtime_decision"
)

// Envelope wraps every message. Seq is monotonic per sender per connection.
type Envelope struct {
	Type    MessageType `json:"type"`
	Seq     uint64      `json:"seq"`
	Version int         `json:"version"`
	Payload any         `json:"payload,omitempty"`
}

// RawEnvelope is an envelope whose payload is decoded later, once the type is known.
type RawEnvelope struct {
	Type    MessageType     `json:"type"`
	Seq     uint64          `json:"seq"`
	Version int             `json:"version"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Encode marshals a message.
func Encode(t MessageType, seq uint64, payload any) ([]byte, error) {
	return json.Marshal(Envelope{Type: t, Seq: seq, Version: Version, Payload: payload})
}

// Decode unmarshals the envelope; call Into for the payload.
func Decode(b []byte) (RawEnvelope, error) {
	var e RawEnvelope
	if err := json.Unmarshal(b, &e); err != nil {
		return e, err
	}
	if e.Type == "" {
		return e, fmt.Errorf("proto: message without type")
	}
	return e, nil
}

// Into decodes the payload into v.
func (e RawEnvelope) Into(v any) error {
	if len(e.Payload) == 0 {
		return fmt.Errorf("proto: %s has no payload", e.Type)
	}
	return json.Unmarshal(e.Payload, v)
}

// Negotiate picks the protocol version for a runner that speaks `runner`.
func Negotiate(runner int) (int, error) {
	if runner < MinSupportedVersion {
		return 0, fmt.Errorf("proto: runner speaks v%d, server needs at least v%d", runner, MinSupportedVersion)
	}
	if runner > Version {
		return Version, nil
	}
	return runner, nil
}

// Register is the first message a runner sends. The runner authenticates with its token
// in the WebSocket handshake (Authorization: Bearer), never inside a message.
type Register struct {
	Name     string `json:"name"`
	Location string `json:"location"` // "vps" | "mac" | "other"
	// Placement is where this runner takes work from: PlacementLocal or PlacementCloud. Empty
	// lets the server derive it from Location, which is what a runner older than the field gets.
	Placement     string       `json:"placement,omitempty"`
	BinaryVersion string       `json:"binary_version"`
	Capabilities  Capabilities `json:"capabilities"`
}

// Placements. A local job runs on a machine of the person who started it; a cloud job on a
// runner the server keeps or starts for it. A runner takes only jobs of its own placement.
const (
	PlacementLocal = "local"
	PlacementCloud = "cloud"
)

// PlacementFor is the placement a runner gets when it does not name one: a VPS is the cloud,
// a Mac or anything else is somebody's own machine.
func PlacementFor(location string) string {
	if location == "vps" {
		return PlacementCloud
	}
	return PlacementLocal
}

// ValidPlacement reports whether p names a placement.
func ValidPlacement(p string) bool { return p == PlacementLocal || p == PlacementCloud }

// Capabilities describes what a runner can execute.
type Capabilities struct {
	Backends    []string `json:"backends"`     // "claude", "acp:codex", "acp:gemini"
	MaxParallel int      `json:"max_parallel"` // concurrent jobs
	Projects    []string `json:"projects"`     // project ids; empty = any project
	// CanLogin says this runner can drive an interactive backend login and answer with a
	// LoginPrompt. A runner that cannot must say so, rather than leaving a person clicking a
	// button that writes one line into a log file nobody reads.
	CanLogin bool `json:"can_login,omitempty"`
	// ReadOnly are the backends that can hold an agent to read-only tools (chat), and NoTools those
	// that can run one with none at all (summaries). A job is leased only to a runner whose backend
	// can keep it, rather than to one that would refuse it. Nil, from a runner before these fields:
	// every backend keeps both, as Claude Code, the only one there was, does.
	ReadOnly []string `json:"read_only,omitempty"`
	NoTools  []string `json:"no_tools,omitempty"`
	// PerBackend is what each backend can do in a live session (v4), by backend name. A backend
	// missing from it (and every backend of a v3 runner) can do none of it: the zero value.
	PerBackend map[string]BackendCapabilities `json:"per_backend,omitempty"`
}

// Enforcement says who holds an agent to its tier: the agent's CLI itself (its own flags and
// permission modes), or the runner answering the agent's permission requests.
const (
	EnforcementNative = "native"
	EnforcementRunner = "runner"
)

// BackendCapabilities is what one backend can do while a job runs (v4). Clients hide an action a
// job's backend cannot carry out rather than offer a button that does nothing.
type BackendCapabilities struct {
	// ActiveSteering: a person's message reaches the running turn without restarting it.
	ActiveSteering bool `json:"active_steering"`
	// SteeringInterruptsTool: such a message interrupts a tool that is running.
	SteeringInterruptsTool bool `json:"steering_interrupts_tool"`
	// NativeInterrupt: stop ends the turn without killing the process.
	NativeInterrupt bool `json:"native_interrupt"`
	// ToolApprovals: the backend asks before a tool and waits for the answer.
	ToolApprovals bool `json:"tool_approvals"`
	// ApprovalsLiveOnly: an approval waiting when the process dies is lost with it.
	ApprovalsLiveOnly bool `json:"approvals_live_only"`
	// StreamsText and StreamsReasoning: text (and reasoning) arrives in pieces, not whole messages.
	StreamsText      bool `json:"streams_text"`
	StreamsReasoning bool `json:"streams_reasoning"`
	// Attachments: files (paths or images) can be given to the agent as input.
	Attachments bool `json:"attachments"`
	// Checkpoints: the runner checkpoints the worktree of this backend's jobs after every turn.
	Checkpoints bool `json:"checkpoints"`
	// Enforcement is EnforcementNative or EnforcementRunner.
	Enforcement string `json:"enforcement,omitempty"`
}

// Registered acknowledges a registration.
type Registered struct {
	RunnerID string `json:"runner_id"`
	Version  int    `json:"version"` // protocol version the server will speak
}

// Heartbeat is sent every HeartbeatInterval. It extends the lease of every job listed in
// ActiveJobs; a leased job the runner no longer lists expires and goes back to the queue.
type Heartbeat struct {
	Load int `json:"load"`
	// AuthState is backend → "ok" | "expired" | "missing" | "no_cli" | "unknown". "no_cli"
	// says the command is not on this runner's PATH, which is a different problem from being
	// logged out and has a different fix.
	AuthState  map[string]string `json:"auth_state"`
	ActiveJobs []string          `json:"active_jobs"`
	// Capabilities are the backends' capabilities as they are now (v4), when they changed since
	// the registration: a backend switched to its fallback mode can do less than it said.
	Capabilities map[string]BackendCapabilities `json:"capabilities,omitempty"`
}

// LeaseRequest asks for up to Slots jobs.
type LeaseRequest struct {
	Slots int `json:"slots"`
}

// Bounds limit a job so a runaway cannot burn a session.
type Bounds struct {
	TimeoutSeconds int     `json:"timeout_seconds"`
	MaxToolCalls   int     `json:"max_tool_calls"`
	MaxCostUSD     float64 `json:"max_cost_usd,omitempty"` // 0 = no cap
}

// Repo tells the runner where to check out code. Absent for jobs that need no repository.
type Repo struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	DefaultBranch string `json:"default_branch"`
	// ReadBranch is what a read-only job reads instead of the default branch: the branch the
	// task's work was pushed to (v3). A branch the remote does not have falls back to the default.
	ReadBranch string `json:"read_branch,omitempty"`
}

// ContextDoc is earlier work a job should read before starting.
type ContextDoc struct {
	Kind  string `json:"kind"` // "brief", "plan", "report", "review", "rollback"
	Phase string `json:"phase"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Kinds of job (v3). Empty is a phase job: what every job was before.
const (
	KindPhase   = "phase"
	KindReply   = "reply"
	KindSummary = "summary"
)

// Tiers of a job (v3): what its agent may touch. Empty is work: what every job was before.
const (
	TierWork = "work"
	TierChat = "chat"
)

// Agent is who does a job, for the runner to sign its commits with (v3).
type Agent struct {
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
}

// Turn is the conversation a reply answers in (v3), in the order a prompt reads it: who asks
// what, the conversation (its background, then the latest messages, words from outside the
// workspace already marked untrusted) and the ask again, briefly.
type Turn struct {
	ID           string       `json:"id"`
	Ask          string       `json:"ask"`
	Conversation []ContextDoc `json:"conversation,omitempty"`
	Repeat       string       `json:"repeat,omitempty"`
}

// Job is one unit of work handed to a runner.
type Job struct {
	JobID           string `json:"job_id"`
	TaskID          string `json:"task_id"`
	TaskTitle       string `json:"task_title"`
	TaskDescription string `json:"task_description,omitempty"`
	// TaskOrigin is "external" when the task's words were written outside the workspace — a
	// webhook, an issue, an alert. The prompt then carries them as data to judge, never as
	// instructions to follow.
	TaskOrigin  string       `json:"task_origin,omitempty"`
	Guide       string       `json:"guide,omitempty"`   // the role's prompt, resolved per project
	Context     []ContextDoc `json:"context,omitempty"` // earlier artifacts and rollback reasons
	Repo        *Repo        `json:"repo,omitempty"`
	ProjectID   string       `json:"project_id"`
	Phase       string       `json:"phase"`
	Role        string       `json:"role"`
	Backend     string       `json:"backend"`
	Model       string       `json:"model,omitempty"` // empty = the backend's default
	Instruction string       `json:"instruction"`
	Bounds      Bounds       `json:"bounds"`
	// AgentToken is the job's own identity for talking back to Gator, for the runner to put
	// in the agent's environment. Empty when the server mints none.
	AgentToken     string    `json:"agent_token,omitempty"`
	Attempt        int       `json:"attempt"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	// Kind, Tier, Agent and Turn are v3.
	Kind  string `json:"kind,omitempty"`
	Tier  string `json:"tier,omitempty"`
	Agent *Agent `json:"agent,omitempty"`
	Turn  *Turn  `json:"turn,omitempty"`
	// Skills are the skills the job's agent carries, each an Agent Skills folder, whole. Additive
	// in v3: a runner that does not know them runs the job without.
	Skills []Skill `json:"skills,omitempty"`
	// MCP are the remote MCP servers the job's agent may reach, by name. The runner reaches each
	// at <server>/mcp/proxy/<name> with the job's own token, and only there: no credential of a
	// remote server is in a job, ever. Additive in v3.
	MCP []string `json:"mcp,omitempty"`
	// NoCheckpoints says the project turned checkpoints off (v4): on a large repository adding
	// every file after every turn can cost more than the diff is worth.
	NoCheckpoints bool `json:"no_checkpoints,omitempty"`
	// Approvals are the tools of a work job that wait for a person (v4), for a backend that can
	// ask (ToolApprovals). Nil: nothing is asked, as before.
	Approvals *Approvals `json:"approvals,omitempty"`
}

// Approvals is the project's approvals policy as a runner holds a job to it: rules to ask a person
// about, rules never allowed, and how long a request waits before it is refused.
type Approvals struct {
	Ask            []string `json:"ask,omitempty"`
	Deny           []string `json:"deny,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// Kinds of a runtime request.
const (
	RequestToolApproval = "tool_approval"
	RequestQuestion     = "question"
)

// MaxRequestInput bounds the input of a tool a runtime request carries.
const MaxRequestInput = 64 << 10

// RuntimeRequest is an agent waiting for a person mid-turn (v4): a tool the approvals policy asks
// about, or a question. ID is the runner's, a UUID: a request sent again is the same request.
type RuntimeRequest struct {
	ID    string          `json:"id"`
	JobID string          `json:"job_id"`
	Kind  string          `json:"kind"`
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// Rule is the rule of the policy that asks; empty for a question.
	Rule      string `json:"rule,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	// TimeoutSeconds is how long the runner waits for the answer.
	TimeoutSeconds int `json:"timeout_seconds"`
}

// Runtime decisions.
const (
	DecisionAllow           = "allow"
	DecisionAllowForSession = "allow_for_session"
	DecisionDeny            = "deny"
	DecisionStop            = "stop"
	DecisionExpired         = "expired"
	DecisionCancelled       = "cancelled"
)

// RuntimeDecision answers a RuntimeRequest. Feedback is why it was denied (the agent reads it) or,
// for a question, the person's answer.
type RuntimeDecision struct {
	ID       string `json:"id"`
	JobID    string `json:"job_id"`
	Decision string `json:"decision"`
	Feedback string `json:"feedback,omitempty"`
	By       string `json:"by,omitempty"`
}

// Allows says whether the decision lets the tool run.
func (d RuntimeDecision) Allows() bool {
	return d.Decision == DecisionAllow || d.Decision == DecisionAllowForSession
}

// Skill is one skill of a job's agent: its SKILL.md and whatever else the folder holds
// (scripts/, references/, assets/). Its scripts are run only by the agent, in the job, with the
// job's own rights.
type Skill struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Files       []SkillFile `json:"files"`
}

// SkillFile is one file of a skill, at its path inside the skill's folder.
type SkillFile struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

// KindOrPhase is the job's kind; a job that names none is a phase job.
func (j Job) KindOrPhase() string {
	if j.Kind == "" {
		return KindPhase
	}
	return j.Kind
}

// ReadOnly says the job's agent gets no tools that write and a checkout it cannot change.
func (j Job) ReadOnly() bool { return j.Tier == TierChat }

// Lease hands jobs to the runner. An empty list means nothing is queued for it.
type Lease struct {
	Jobs []Job `json:"jobs"`
}

// JobEvent is one item of agent output (text, tool call, commit, screenshot).
type JobEvent struct {
	Seq     uint64          `json:"seq"`
	Type    string          `json:"type"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Events streams output for one job. Per-job event seq makes resends idempotent.
type Events struct {
	JobID  string     `json:"job_id"`
	Events []JobEvent `json:"events"`
}

// Ack confirms the server durably processed the runner message with this envelope seq.
// The runner keeps events and finishes until acked and resends them after reconnect.
type Ack struct {
	Seq uint64 `json:"seq"`
}

// Steer injects a correction into a running job.
type Steer struct {
	JobID   string `json:"job_id"`
	Message string `json:"message"`
}

// Stop asks the runner to end a job; it answers with a Finish whose status is "stopped".
type Stop struct {
	JobID  string `json:"job_id"`
	Reason string `json:"reason"`
}

// LoginBackend asks the runner to start an interactive login for a backend.
type LoginBackend struct {
	Backend string `json:"backend"`
}

// LoginPrompt returns what the person must open to finish the login.
type LoginPrompt struct {
	Backend string `json:"backend"`
	URL     string `json:"url"`
	Code    string `json:"code,omitempty"`
}

// Usage is what one job consumed. Every finish carries it; the server stores it per task
// and phase so time and tokens can be reported for every task Gator runs.
type Usage struct {
	Backend          string    `json:"backend"` // "claude" | "acp:<agent>"
	Model            string    `json:"model"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	CostEstimated    bool      `json:"cost_estimated"` // true on subscriptions: priced from list rates, not billed
	DurationMS       int64     `json:"duration_ms"`    // wall time the agent process ran
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
}

// Reported says whether the usage carries any measurement at all.
func (u Usage) Reported() bool {
	return u.DurationMS > 0 || u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0
}

// Finish statuses.
const (
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusStopped = "stopped"
)

// Finish is the receipt a runner sends when a job ends. The server does not accept
// `done` without usage: a done job with no measurement is recorded as failed.
type Finish struct {
	JobID        string   `json:"job_id"`
	Status       string   `json:"status"` // StatusDone | StatusFailed | StatusStopped
	StopReason   string   `json:"stop_reason,omitempty"`
	ExitCode     int      `json:"exit_code"`
	Branch       string   `json:"branch,omitempty"`
	Commits      []string `json:"commits,omitempty"`
	ChangedFiles int      `json:"changed_files"`
	// ChangedPaths are the files the job actually committed, bounded. The catalogue is built
	// from where work really happens, and a count cannot say where that was.
	ChangedPaths []string `json:"changed_paths,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	Digest       *Digest  `json:"digest,omitempty"`
	Usage        Usage    `json:"usage"`
	// Objections counts the times this turn was sent back to work before it was allowed to
	// end. A receipt that hides them would make a two-hour job look like a one-hour one.
	Objections int `json:"objections,omitempty"`
	// Checkpoints are the turns' checkpoints the runner sent (v4), counted; their diffs went
	// before, each in its own checkpoint message.
	Checkpoints []CheckpointSummary `json:"checkpoints,omitempty"`
}

// Checkpoint bounds: each diff a checkpoint carries is cut at MaxDiffBytes (both fit one runner
// message, base64 and all), and at most MaxCheckpointFiles files are listed.
const (
	MaxDiffBytes       = 1 << 20
	MaxCheckpointFiles = 1000
)

// File statuses in a checkpoint.
const (
	FileAdded    = "added"
	FileModified = "modified"
	FileDeleted  = "deleted"
)

// FileChange is one file a turn changed, by how much. A binary file is listed as changed, with no
// line counts and nothing of it in the diff.
type FileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Status  string `json:"status"`
	Binary  bool   `json:"binary,omitempty"`
}

// Checkpoint is the worktree of a job after one of its turns (v4): a commit the runner made on a
// hidden ref, without touching the agent's branch or index, and what changed since the turn
// before — Seq 1 is the first turn, against the worktree as the job found it. Whole is the job
// so far (the worktree as the job found it, to this turn), for a view of all of it.
type Checkpoint struct {
	JobID      string       `json:"job_id"`
	Repo       string       `json:"repo"`
	Seq        int          `json:"seq"`
	BaseCommit string       `json:"base_commit"`
	HeadCommit string       `json:"head_commit"`
	Files      []FileChange `json:"files"`
	Diff       []byte       `json:"diff,omitempty"`
	Truncated  bool         `json:"truncated,omitempty"`
	WholeFiles []FileChange `json:"whole_files,omitempty"`
	Whole      []byte       `json:"whole,omitempty"`
	// WholeTruncated says Whole was cut at MaxDiffBytes.
	WholeTruncated bool `json:"whole_truncated,omitempty"`
	// TookMS is how long the checkpoint took: on a large repository it is the price of the feature.
	TookMS int64 `json:"took_ms"`
}

// CheckpointSummary is a checkpoint as the receipt counts it.
type CheckpointSummary struct {
	Seq     int `json:"seq"`
	Files   int `json:"files"`
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

// Summary counts what a checkpoint's turn changed.
func (c Checkpoint) Summary() CheckpointSummary {
	out := CheckpointSummary{Seq: c.Seq, Files: len(c.Files)}
	for _, f := range c.Files {
		out.Added += f.Added
		out.Removed += f.Removed
	}
	return out
}

// TurnEnding is the runner asking, before it sends a Finish, whether the turn may end. The
// agent has said it is done; the question is whether anything watching the work disagrees.
// A runner that does not ask is not overruled: silence on this exchange means yes.
type TurnEnding struct {
	JobID string `json:"job_id"`
	// Turn counts the endings of this job: 1 is the first, 2 the one after an objection.
	Turn         int      `json:"turn"`
	Status       string   `json:"status"`
	Summary      string   `json:"summary,omitempty"`
	Commits      []string `json:"commits,omitempty"`
	ChangedPaths []string `json:"changed_paths,omitempty"`
	Digest       *Digest  `json:"digest,omitempty"`
}

// Continue answers a TurnEnding. No objections means the turn may end; the server sends
// exactly one answer per question, including when it has run out of objections to allow.
type Continue struct {
	JobID      string      `json:"job_id"`
	Objections []Objection `json:"objections,omitempty"`
	// Exhausted says something still objects but this job has spent its objections, so the
	// turn ends regardless. Note carries what went unheard, for the record.
	Exhausted bool   `json:"exhausted,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Objection is one reason the work is not finished. Source names who says so — a plugin by
// name, or "gator" for the core. The reason is untrusted text: it reaches the agent as data.
type Objection struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// Digest is the structured account of one session: what changed, what was decided and
// rejected, and what is left. The server folds every digest of a task into its working state.
type Digest struct {
	Changes   []string `json:"changes"`
	Decisions []string `json:"decisions"`
	Rejected  []string `json:"rejected"`
	Left      []string `json:"left"`
}

// Error is sent by the server when a message is rejected.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"` // the server closes the connection after a fatal error
}
