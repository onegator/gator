// Package proto is the only package shared between gator-server and gator-runner.
// It defines the wire contract of the runner WebSocket and is versioned independently
// of either binary: the server accepts protocol versions N and N-1.
package proto

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

// Error is sent by the server when a message is rejected.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
