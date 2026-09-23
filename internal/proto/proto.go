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
const Version = 2

// MinSupportedVersion is the oldest runner protocol the server still speaks.
const MinSupportedVersion = 1

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
	Name          string       `json:"name"`
	Location      string       `json:"location"` // "vps" | "mac" | "other"
	BinaryVersion string       `json:"binary_version"`
	Capabilities  Capabilities `json:"capabilities"`
}

// Capabilities describes what a runner can execute.
type Capabilities struct {
	Backends    []string `json:"backends"`     // "claude", "codex", "pi"
	MaxParallel int      `json:"max_parallel"` // concurrent jobs
	Projects    []string `json:"projects"`     // project ids; empty = any project
	// CanLogin says this runner can drive an interactive backend login and answer with a
	// LoginPrompt. A runner that cannot must say so, rather than leaving a person clicking a
	// button that writes one line into a log file nobody reads.
	CanLogin bool `json:"can_login,omitempty"`
}

// Registered acknowledges a registration.
type Registered struct {
	RunnerID string `json:"runner_id"`
	Version  int    `json:"version"` // protocol version the server will speak
}

// Heartbeat is sent every HeartbeatInterval. It extends the lease of every job listed in
// ActiveJobs; a leased job the runner no longer lists expires and goes back to the queue.
type Heartbeat struct {
	Load       int               `json:"load"`
	AuthState  map[string]string `json:"auth_state"` // backend → "ok" | "expired" | "missing"
	ActiveJobs []string          `json:"active_jobs"`
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
}

// ContextDoc is earlier work a job should read before starting.
type ContextDoc struct {
	Kind  string `json:"kind"` // "brief", "plan", "report", "review", "rollback"
	Phase string `json:"phase"`
	Title string `json:"title"`
	Body  string `json:"body"`
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
}

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
	Backend          string    `json:"backend"` // "claude" | "codex" | "pi"
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
