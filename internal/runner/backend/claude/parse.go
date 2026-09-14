// Package claude drives Claude Code headless (`claude -p --output-format stream-json`) and
// turns its NDJSON stream into job events, a result and usage.
package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Result is the final `result` line of a session.
type Result struct {
	Subtype        string // "success", "error_max_turns", "error_during_execution", …
	IsError        bool
	Text           string // the agent's final answer
	SessionID      string
	NumTurns       int
	DurationMS     int64
	TerminalReason string
	Errors         []string
	CostUSD        float64
	CostEstimated  bool // modelUsage costBasis "list": priced from list rates, not billed
	Tokens         Tokens
}

// Tokens are summed over every model the session used (Claude Code also calls smaller
// models in the background, which the top-level usage block leaves out).
type Tokens struct {
	Input, Output, CacheRead, CacheWrite int64
}

// Parser consumes stream-json lines. It is pure: no I/O besides the emit callback.
type Parser struct {
	SessionID  string
	Model      string
	Version    string
	ToolCalls  int
	Result     *Result
	BadLines   int // lines that were not JSON
	Unknown    int // JSON lines of a type this parser does not know (ignored for forward compatibility)
	MaxPayload int // truncate long strings in event payloads; default 4096
}

// ErrNotJSON is returned for a line that is not a JSON object.
var ErrNotJSON = errors.New("claude: line is not JSON")

type line struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Model     string          `json:"model"`
	Version   string          `json:"claude_code_version"`
	Message   json.RawMessage `json:"message"`
	RateLimit *rateLimit      `json:"rate_limit_info"`

	// result fields
	IsError        bool                  `json:"is_error"`
	Result         string                `json:"result"`
	NumTurns       int                   `json:"num_turns"`
	DurationMS     int64                 `json:"duration_ms"`
	TotalCost      float64               `json:"total_cost_usd"`
	TerminalReason string                `json:"terminal_reason"`
	Errors         []string              `json:"errors"`
	Usage          usage                 `json:"usage"`
	ModelUsage     map[string]modelUsage `json:"modelUsage"`
}

type rateLimit struct {
	Status        string `json:"status"`
	RateLimitType string `json:"rateLimitType"`
	ResetsAt      int64  `json:"resetsAt"`
	Windows       map[string]struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    int64   `json:"resetsAt"`
	} `json:"unifiedWindows"`
}

type usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

type modelUsage struct {
	Input      int64   `json:"inputTokens"`
	Output     int64   `json:"outputTokens"`
	CacheRead  int64   `json:"cacheReadInputTokens"`
	CacheWrite int64   `json:"cacheCreationInputTokens"`
	CostUSD    float64 `json:"costUSD"`
	CostBasis  string  `json:"costBasis"`
}

type message struct {
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// Line parses one line and emits events for it.
func (p *Parser) Line(raw []byte, emit func(typ string, payload any)) error {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil
	}
	var l line
	if !strings.HasPrefix(s, "{") || json.Unmarshal([]byte(s), &l) != nil {
		p.BadLines++
		return ErrNotJSON
	}
	if l.SessionID != "" {
		p.SessionID = l.SessionID
	}
	switch l.Type {
	case "system":
		if l.Subtype == "init" {
			p.Model, p.Version = l.Model, l.Version
			emit("session", map[string]any{"session_id": l.SessionID, "model": l.Model, "claude_code_version": l.Version})
		}
		// hook_* lines carry the host's hook output and are deliberately not forwarded.
	case "rate_limit_event":
		if l.RateLimit != nil {
			w := map[string]float64{}
			for k, v := range l.RateLimit.Windows {
				w[k] = v.Utilization
			}
			emit("rate_limit", map[string]any{"status": l.RateLimit.Status, "type": l.RateLimit.RateLimitType, "resets_at": l.RateLimit.ResetsAt, "utilization": w})
		}
	case "assistant":
		var m message
		if err := json.Unmarshal(l.Message, &m); err != nil {
			p.Unknown++
			return nil
		}
		if p.Model == "" {
			p.Model = m.Model
		}
		for _, b := range blocks(m.Content) {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					emit("text", map[string]any{"text": p.cut(b.Text)})
				}
			case "tool_use":
				p.ToolCalls++
				emit("tool_call", map[string]any{"id": b.ID, "name": b.Name, "input": p.cutJSON(b.Input)})
			}
			// thinking blocks are not forwarded
		}
	case "user":
		var m message
		if err := json.Unmarshal(l.Message, &m); err != nil {
			p.Unknown++
			return nil
		}
		for _, b := range blocks(m.Content) {
			if b.Type == "tool_result" {
				emit("tool_result", map[string]any{"tool_use_id": b.ToolUseID, "is_error": b.IsError, "content": p.cut(flatten(b.Content))})
			}
		}
	case "result":
		r := &Result{
			Subtype: l.Subtype, IsError: l.IsError, Text: l.Result, SessionID: l.SessionID, NumTurns: l.NumTurns,
			DurationMS: l.DurationMS, TerminalReason: l.TerminalReason, Errors: l.Errors, CostUSD: l.TotalCost, CostEstimated: true,
		}
		if len(l.ModelUsage) > 0 {
			billed := false
			for _, mu := range l.ModelUsage {
				r.Tokens.Input += mu.Input
				r.Tokens.Output += mu.Output
				r.Tokens.CacheRead += mu.CacheRead
				r.Tokens.CacheWrite += mu.CacheWrite
				if mu.CostBasis != "" && mu.CostBasis != "list" {
					billed = true
				}
			}
			r.CostEstimated = !billed
		} else {
			r.Tokens = Tokens{Input: l.Usage.Input, Output: l.Usage.Output, CacheRead: l.Usage.CacheRead, CacheWrite: l.Usage.CacheWrite}
		}
		p.Result = r
		emit("result", map[string]any{"subtype": r.Subtype, "is_error": r.IsError, "num_turns": r.NumTurns, "terminal_reason": r.TerminalReason, "cost_usd": r.CostUSD})
	default:
		p.Unknown++
	}
	return nil
}

// blocks decodes message content, which is either a list of blocks or a plain string.
func blocks(raw json.RawMessage) []block {
	var bs []block
	if json.Unmarshal(raw, &bs) == nil {
		return bs
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return []block{{Type: "text", Text: s}}
	}
	return nil
}

// flatten turns tool_result content (string or list of text blocks) into text.
func flatten(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []string
	for _, b := range blocks(raw) {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else if b.Type != "" {
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

func (p *Parser) limit() int {
	if p.MaxPayload <= 0 {
		return 4096
	}
	return p.MaxPayload
}

func (p *Parser) cut(s string) string {
	if n := p.limit(); len(s) > n {
		return s[:n] + fmt.Sprintf("… [%d bytes truncated]", len(s)-n)
	}
	return s
}

// cutJSON keeps a tool input readable but bounded: long string fields are truncated.
func (p *Parser) cutJSON(raw json.RawMessage) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return p.cut(string(raw))
	}
	return p.trimValue(v)
}

func (p *Parser) trimValue(v any) any {
	switch x := v.(type) {
	case string:
		return p.cut(x)
	case map[string]any:
		for k, vv := range x {
			x[k] = p.trimValue(vv)
		}
		return x
	case []any:
		for i := range x {
			x[i] = p.trimValue(x[i])
		}
		return x
	}
	return v
}
