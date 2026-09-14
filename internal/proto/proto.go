// Package proto is the only package shared between gator-server and gator-runner.
// It defines the wire contract of the runner WebSocket and is versioned independently
// of either binary: the server accepts protocol versions N and N-1.
package proto

import "time"

// Version is the current runner protocol version. Bump on any incompatible change
// and keep the previous version readable on the server side.
const Version = 1

// MinSupportedVersion is the oldest runner protocol the server still speaks.
const MinSupportedVersion = 1

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
)

// Server → runner.
const (
	TypeRegistered   MessageType = "registered"
	TypeLease        MessageType = "lease"
	TypeSteer        MessageType = "steer"
	TypeStop         MessageType = "stop"
	TypeLoginBackend MessageType = "login_backend"
	TypeError        MessageType = "error"
)

// Envelope wraps every message. Seq is monotonic per sender per connection.
type Envelope struct {
	Type    MessageType `json:"type"`
	Seq     uint64      `json:"seq"`
	Version int         `json:"version"`
	Payload any         `json:"payload,omitempty"`
}

// Register is the first message a runner sends.
type Register struct {
	Token        string       `json:"token"`
	Name         string       `json:"name"`
	Location     string       `json:"location"` // "vps" | "mac"
	Capabilities Capabilities `json:"capabilities"`
}

// Capabilities describes what a runner can execute.
type Capabilities struct {
	Backends    []string `json:"backends"`     // "claude", "codex", "pi"
	MaxParallel int      `json:"max_parallel"` // concurrent jobs
	Projects    []string `json:"projects"`     // empty = any project
}

// Registered acknowledges a registration.
type Registered struct {
	RunnerID string `json:"runner_id"`
	Version  int    `json:"version"` // protocol version the server will speak
}

// Heartbeat is sent every 15s and extends every active lease.
type Heartbeat struct {
	Load       int               `json:"load"`
	AuthState  map[string]string `json:"auth_state"` // backend → "ok" | "expired" | "missing"
	ActiveJobs []string          `json:"active_jobs"`
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

// Finish is the receipt a runner sends when a job ends. The server does not accept
// `done` without it.
type Finish struct {
	JobID        string   `json:"job_id"`
	Status       string   `json:"status"` // "done" | "failed" | "stopped"
	StopReason   string   `json:"stop_reason,omitempty"`
	ExitCode     int      `json:"exit_code"`
	Branch       string   `json:"branch,omitempty"`
	Commits      []string `json:"commits,omitempty"`
	ChangedFiles int      `json:"changed_files"`
	SessionID    string   `json:"session_id,omitempty"`
	Usage        Usage    `json:"usage"`
}

// Error is sent by the server when a message is rejected.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
