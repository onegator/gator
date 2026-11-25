// Package a2a connects Gator's agents with agents outside it through A2A (Agent2Agent), the Linux
// Foundation protocol: an agent is described by an Agent Card (name, skills, endpoint, how to
// authenticate) and is given tasks over JSON-RPC 2.0 on HTTP, each with a lifecycle (submitted,
// working, completed…) and the agent's answer.
//
// Outbound, a project's remote agent (agents.kind = 'remote') is a member like any other: a mention
// is a turn, and the turn is an A2A task sent to the endpoint its card names, with a credential
// kept here. Inbound, a project's admin gives an A2A client a token and a daily budget, and the
// client asks the agents the project exposes; each ask is a message in #a2a-<client>, where people
// see who asked what, and its words are an outside identity's, answered in chat.
//
// This is a thin layer on JSON-RPC rather than an SDK: the subset of A2A 0.3 a task needs
// (message/send, tasks/get, tasks/cancel refused), without streaming or push notifications.
package a2a

import (
	"encoding/json"
	"strings"

	"github.com/onegator/gator/internal/server/bridges"
)

// ProtocolVersion is the A2A version the cards say.
const ProtocolVersion = "0.3.0"

// CardPath is where an agent's card is, under its base address (A2A 0.3: well-known agent-card.json).
const CardPath = "/.well-known/agent-card.json"

// AgentCard describes an agent.
type AgentCard struct {
	ProtocolVersion    string                    `json:"protocolVersion"`
	Name               string                    `json:"name"`
	Description        string                    `json:"description"`
	URL                string                    `json:"url"`
	PreferredTransport string                    `json:"preferredTransport,omitempty"`
	Version            string                    `json:"version"`
	Capabilities       Capabilities              `json:"capabilities"`
	DefaultInputModes  []string                  `json:"defaultInputModes"`
	DefaultOutputModes []string                  `json:"defaultOutputModes"`
	Skills             []Skill                   `json:"skills"`
	SecuritySchemes    map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	Security           []map[string][]string     `json:"security,omitempty"`
}

// Capabilities are what an agent's endpoint does beyond message/send.
type Capabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
}

// Skill is one thing an agent is good at.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
}

// SecurityScheme is how a client authenticates; Gator speaks HTTP bearer.
type SecurityScheme struct {
	Type   string `json:"type"`
	Scheme string `json:"scheme,omitempty"`
}

// Message is one turn of a conversation with an agent.
type Message struct {
	Kind      string `json:"kind"` // message
	Role      string `json:"role"` // user | agent
	MessageID string `json:"messageId"`
	Parts     []Part `json:"parts"`
	ContextID string `json:"contextId,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
}

// Part is a piece of a message; Gator reads and writes text.
type Part struct {
	Kind string `json:"kind"` // text | file | data
	Text string `json:"text,omitempty"`
}

// Text is what a message's text parts say.
func (m Message) Text() string { return partsText(m.Parts) }

func partsText(parts []Part) string {
	var out []string
	for _, p := range parts {
		if p.Kind == "text" && strings.TrimSpace(p.Text) != "" {
			out = append(out, strings.TrimSpace(p.Text))
		}
	}
	return strings.Join(out, "\n\n")
}

// Task states.
const (
	StateSubmitted     = "submitted"
	StateWorking       = "working"
	StateInputRequired = "input-required"
	StateCompleted     = "completed"
	StateCanceled      = "canceled"
	StateFailed        = "failed"
	StateRejected      = "rejected"
	StateAuthRequired  = "auth-required"
	StateUnknown       = "unknown"
)

// final says whether a task in state s is over.
func final(s string) bool {
	switch s {
	case StateCompleted, StateCanceled, StateFailed, StateRejected, StateInputRequired, StateAuthRequired:
		return true
	}
	return false
}

// Task is a unit of work an agent was given.
type Task struct {
	Kind      string         `json:"kind"` // task
	ID        string         `json:"id"`
	ContextID string         `json:"contextId"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// TaskStatus is where a task stands, with what the agent last said.
type TaskStatus struct {
	State     string   `json:"state"`
	Message   *Message `json:"message,omitempty"`
	Timestamp string   `json:"timestamp,omitempty"`
}

// Artifact is something a task produced.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name,omitempty"`
	Parts      []Part `json:"parts"`
}

// Answer is what a task says: its artifacts' text, else its status message's.
func (t Task) Answer() string {
	var out []string
	for _, a := range t.Artifacts {
		if s := partsText(a.Parts); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 && t.Status.Message != nil {
		return t.Status.Message.Text()
	}
	return strings.Join(out, "\n\n")
}

// SendParams is message/send.
type SendParams struct {
	Message       Message         `json:"message"`
	Configuration *SendConfig     `json:"configuration,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

// SendConfig asks for an answer in the same call (blocking) or not.
type SendConfig struct {
	Blocking            bool     `json:"blocking,omitempty"`
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
}

// TaskQuery is tasks/get and tasks/cancel.
type TaskQuery struct {
	ID string `json:"id"`
}

// JSON-RPC 2.0, as A2A carries it.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return "a2a: " + e.Message }

// JSON-RPC and A2A error codes.
const (
	codeParse            = -32700
	codeInvalidRequest   = -32600
	codeMethodNotFound   = -32601
	codeInvalidParams    = -32602
	codeInternal         = -32603
	codeTaskNotFound     = -32001
	codeNotCancelable    = -32002
	codeUnsupported      = -32004
	codeContentType      = -32005
	codeBudget           = -32010 // Gator's own: the client's daily budget is spent
	codeRateLimited      = -32011 // Gator's own: too many requests
	methodSend           = "message/send"
	methodStream         = "message/stream"
	methodGetTask        = "tasks/get"
	methodCancelTask     = "tasks/cancel"
	methodResubscribe    = "tasks/resubscribe"
	maxRequestBytes      = 1 << 20
	maxResponseBytes     = 4 << 20
	maxTextRunes         = 40000
	defaultDailyBudget   = 5
	defaultPerMinute     = 30
	providerA2A          = bridges.ProviderA2A
	channelPrefix        = "a2a-"
	bearerSchemeName     = "bearer"
	jsonRPCTransportName = "JSONRPC"
)
