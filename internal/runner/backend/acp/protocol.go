// Package acp runs any agent that speaks the Agent Client Protocol (ACP) — Codex, Gemini CLI,
// OpenCode, Claude Code through its adapter — as a runner backend. ACP is JSON-RPC 2.0 over the
// agent's stdio: the runner, as the client, opens a session in the job's worktree with Gator's MCP
// servers, sends the prompt, and reads the agent's updates (its words, its tool calls and their
// results) as a stream. The agent asks the client before it uses a tool that needs permission;
// the answer is the job's tier, decided here and not by the agent's own defaults.
//
// Only the part of the protocol a headless job needs is implemented: no file system or terminal
// is offered to the agent (it uses its own tools), and no interactive authentication — a
// backend that is not logged in reports so and gets no job.
package acp

import "encoding/json"

// ProtocolVersion is the ACP major version this client speaks.
const ProtocolVersion = 1

// ACP methods the client calls (client → agent).
const (
	methodInitialize = "initialize"
	methodNewSession = "session/new"
	methodLoad       = "session/load"
	methodPrompt     = "session/prompt"
	methodCancel     = "session/cancel" // notification
	methodSetMode    = "session/set_mode"
	methodSetModel   = "session/set_model"
)

// ACP methods the agent calls (agent → client).
const (
	methodUpdate     = "session/update" // notification
	methodPermission = "session/request_permission"
)

type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
	ClientInfo         *implementation    `json:"clientInfo,omitempty"`
}

// clientCapabilities offers the agent nothing of the client's own: it reads, writes and runs
// commands with its own tools, under the permission answers below.
type clientCapabilities struct {
	FS       fsCapabilities `json:"fs"`
	Terminal bool           `json:"terminal"`
}

type fsCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AuthMethods       []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"authMethods"`
	AgentInfo *implementation `json:"agentInfo,omitempty"`
}

type agentCapabilities struct {
	LoadSession     bool `json:"loadSession"`
	MCPCapabilities struct {
		HTTP bool `json:"http"`
		SSE  bool `json:"sse"`
	} `json:"mcpCapabilities"`
}

// mcpServer is an MCP server over HTTP as session/new takes it.
type mcpServer struct {
	Type    string      `json:"type"`
	Name    string      `json:"name"`
	URL     string      `json:"url"`
	Headers []nameValue `json:"headers"`
}

type nameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type newSessionParams struct {
	CWD        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers"`
}

type loadSessionParams struct {
	SessionID  string      `json:"sessionId"`
	CWD        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers"`
}

type newSessionResult struct {
	SessionID string       `json:"sessionId"`
	Modes     *modeState   `json:"modes,omitempty"`
	Models    *modelsState `json:"models,omitempty"`
}

// loadSessionResult carries the same modes and models; the session id is the one asked for.
type loadSessionResult struct {
	Modes  *modeState   `json:"modes,omitempty"`
	Models *modelsState `json:"models,omitempty"`
}

type modeState struct {
	CurrentModeID  string `json:"currentModeId"`
	AvailableModes []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"availableModes"`
}

func (m *modeState) has(id string) bool {
	if m == nil || id == "" {
		return false
	}
	for _, x := range m.AvailableModes {
		if x.ID == id {
			return true
		}
	}
	return false
}

type modelsState struct {
	CurrentModelID  string `json:"currentModelId"`
	AvailableModels []struct {
		ModelID string `json:"modelId"`
		Name    string `json:"name"`
	} `json:"availableModels"`
}

func (m *modelsState) has(id string) bool {
	if m == nil || id == "" {
		return false
	}
	for _, x := range m.AvailableModels {
		if x.ModelID == id {
			return true
		}
	}
	return false
}

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Stop reasons of a prompt turn.
const (
	stopEndTurn   = "end_turn"
	stopMaxTokens = "max_tokens"
	stopMaxTurns  = "max_turn_requests"
	stopRefusal   = "refusal"
	stopCancelled = "cancelled"
)

type promptResult struct {
	StopReason string     `json:"stopReason"`
	Usage      *usageInfo `json:"usage,omitempty"`
}

// usageInfo is what an agent reports of the tokens of a prompt turn, where it does (the
// protocol's usage extension; not every adapter sends it).
type usageInfo struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	CachedReadTokens  int64 `json:"cachedReadTokens"`
	CachedWriteTokens int64 `json:"cachedWriteTokens"`
	ThoughtTokens     int64 `json:"thoughtTokens"`
	TotalTokens       int64 `json:"totalTokens"`
}

type cancelParams struct {
	SessionID string `json:"sessionId"`
}

type setModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type setModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

type updateParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// update is one session/update; which fields are set depends on SessionUpdate.
type update struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content,omitempty"`
	ToolCallID    string          `json:"toolCallId,omitempty"`
	Title         string          `json:"title,omitempty"`
	Kind          string          `json:"kind,omitempty"`
	Status        string          `json:"status,omitempty"`
	RawInput      json.RawMessage `json:"rawInput,omitempty"`
	RawOutput     json.RawMessage `json:"rawOutput,omitempty"`
	// current_mode_update
	CurrentModeID string `json:"currentModeId,omitempty"`
	// usage_update
	Used int64     `json:"used,omitempty"`
	Size int64     `json:"size,omitempty"`
	Cost *costInfo `json:"cost,omitempty"`
}

type costInfo struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// Session update kinds.
const (
	updateMessageChunk  = "agent_message_chunk"
	updateThoughtChunk  = "agent_thought_chunk"
	updateUserChunk     = "user_message_chunk"
	updateToolCall      = "tool_call"
	updateToolCallState = "tool_call_update"
	updatePlan          = "plan"
	updateUsage         = "usage_update"
	updateMode          = "current_mode_update"
)

// Tool call statuses.
const (
	toolCompleted = "completed"
	toolFailed    = "failed"
)

type permissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  permissionToolCall `json:"toolCall"`
	Options   []permissionOption `json:"options"`
}

type permissionToolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // allow_once | allow_always | reject_once | reject_always
}

type permissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

type permissionOutcome struct {
	Outcome  string `json:"outcome"` // selected | cancelled
	OptionID string `json:"optionId,omitempty"`
}

// text is the text of a content block, or of a tool call's content list.
func text(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var b contentBlock
	if json.Unmarshal(raw, &b) == nil && b.Type != "" {
		if b.Type == "text" {
			return b.Text
		}
		return "[" + b.Type + "]"
	}
	// A tool call's content: [{type: "content", content: {type: "text", …}}, {type: "diff", …}].
	var list []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Path    string          `json:"path"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return ""
	}
	out := ""
	for _, x := range list {
		var s string
		switch x.Type {
		case "content":
			s = text(x.Content)
		case "diff":
			s = "[diff " + x.Path + "]"
		case "terminal":
			s = "[terminal]"
		}
		if s == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += s
	}
	return out
}
