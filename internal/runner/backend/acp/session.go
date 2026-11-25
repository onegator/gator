package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/version"
)

// Limits a run is held to by the runner itself: an agent cannot be told to stop at a number of
// tool calls or dollars, so the client counts and cancels.
var (
	errToolLimit = errors.New("tool call limit reached")
	errCostLimit = errors.New("cost limit reached")
)

// maxPayload bounds long strings in event payloads, as the Claude backend does.
const maxPayload = 4096

// maxSummary bounds the final answer carried in the receipt.
const maxSummary = 200_000

// setupTimeout bounds each step before the prompt: an agent that does not answer initialize in
// this long is not going to do the job.
const setupTimeout = time.Minute

// driver runs one job's prompt over an ACP connection and turns what the agent says into job
// events. It is the protocol half of the backend; the process half is in backend.go.
type driver struct {
	agent   string
	profile Profile
	perms   Permissions
	emit    backend.Emit
	spec    backend.Spec
	// stop cancels the run when the agent goes over a limit the runner holds it to.
	stop func(error)

	mu        sync.Mutex
	sessionID string
	loading   bool // session/load replays the session's history, which is not this run's
	toolCalls int
	calls     map[string]bool // tool calls seen
	results   map[string]bool // tool calls whose result was told
	pending   strings.Builder // the agent's words not yet emitted: an unfinished paragraph
	segments  []string        // everything the agent said this run, in paragraphs
	sinceTool int             // segments said after the last tool call: the final answer
	costUSD   float64
	costKnown bool
}

func newDriver(agent string, profile Profile, spec backend.Spec, emit backend.Emit, stop func(error)) *driver {
	names := make([]string, 0, len(spec.MCP))
	for _, s := range spec.MCP {
		names = append(names, s.Name)
	}
	return &driver{agent: agent, profile: profile, spec: spec, emit: emit, stop: stop,
		perms: Permissions{Access: spec.Access, MCP: names}, calls: map[string]bool{}, results: map[string]bool{}}
}

// handle serves what the agent sends.
func (d *driver) handle(method string, params json.RawMessage, request bool) (any, error) {
	switch method {
	case methodUpdate:
		var p updateParams
		if json.Unmarshal(params, &p) == nil {
			d.update(p)
		}
		return nil, nil
	case methodPermission:
		var p permissionParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}
		return d.permission(p), nil
	}
	// The client offers no file system and no terminal: the agent uses its own tools.
	if !request {
		return nil, nil
	}
	return nil, &RPCError{Code: codeMethodNotFound, Message: fmt.Sprintf("method %q is not offered by this client", method)}
}

func (d *driver) update(p updateParams) {
	var u update
	if json.Unmarshal(p.Update, &u) != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loading || (d.sessionID != "" && p.SessionID != "" && p.SessionID != d.sessionID) {
		return
	}
	switch u.SessionUpdate {
	case updateMessageChunk:
		d.pending.WriteString(text(u.Content))
		d.flushParagraphs(false)
	case updateToolCall:
		d.flushParagraphs(true)
		d.sinceTool = len(d.segments)
		if u.ToolCallID != "" && !d.calls[u.ToolCallID] {
			d.calls[u.ToolCallID] = true
			d.toolCalls++
			d.emit("tool_call", map[string]any{"id": u.ToolCallID, "name": u.Title, "kind": u.Kind, "input": trimJSON(u.RawInput)})
			if max := d.spec.MaxToolCalls; max > 0 && d.toolCalls > max {
				d.stop(errToolLimit)
			}
		}
		d.result(u)
	case updateToolCallState:
		d.result(u)
	case updatePlan:
		var plan struct {
			Entries []struct {
				Content string `json:"content"`
				Status  string `json:"status"`
			} `json:"entries"`
		}
		if json.Unmarshal(p.Update, &plan) == nil && len(plan.Entries) > 0 {
			d.emit("plan", map[string]any{"entries": plan.Entries})
		}
	case updateUsage:
		if u.Cost != nil && strings.EqualFold(u.Cost.Currency, "USD") {
			d.costUSD, d.costKnown = u.Cost.Amount, true
			if max := d.spec.MaxCostUSD; max > 0 && d.costUSD > max {
				d.stop(errCostLimit)
			}
		}
		// agent_thought_chunk is not forwarded, like Claude's thinking; user_message_chunk is the
		// prompt the client sent, and the rest (commands, modes) are the agent's own business.
	}
}

// result tells a tool call's result once it has one.
func (d *driver) result(u update) {
	if u.ToolCallID == "" || d.results[u.ToolCallID] || (u.Status != toolCompleted && u.Status != toolFailed) {
		return
	}
	d.results[u.ToolCallID] = true
	content := text(u.Content)
	if content == "" && len(u.RawOutput) > 0 {
		content = string(u.RawOutput)
	}
	d.emit("tool_result", map[string]any{"tool_use_id": u.ToolCallID, "is_error": u.Status == toolFailed, "content": cut(content)})
}

// flushParagraphs emits the agent's words a paragraph at a time, as they complete: a reply
// streams into its message while the agent writes, and each text event stays whole, which is how
// the server joins them. all emits what is left too, at a tool call or the end of the turn.
func (d *driver) flushParagraphs(all bool) {
	s := d.pending.String()
	cutAt := len(s)
	if !all {
		cutAt = strings.LastIndex(s, "\n\n")
		if cutAt < 0 {
			return
		}
	}
	done, rest := s[:cutAt], s[cutAt:]
	if !all {
		rest = strings.TrimLeft(rest, "\n")
	}
	d.pending.Reset()
	d.pending.WriteString(rest)
	if strings.TrimSpace(done) == "" {
		return
	}
	d.segments = append(d.segments, strings.TrimSpace(done))
	d.emit("text", map[string]any{"text": cut(done)})
}

// permission answers one request for the job's access, and tells the job's events about it.
func (d *driver) permission(p permissionParams) permissionResult {
	v := d.perms.Decide(p.ToolCall)
	d.mu.Lock()
	if max := d.spec.MaxToolCalls; v.Allow && max > 0 && d.toolCalls > max {
		v = Verdict{Reason: "tool call limit reached"}
	}
	d.mu.Unlock()
	d.emit("permission", map[string]any{"tool_call_id": p.ToolCall.ToolCallID, "title": p.ToolCall.Title,
		"kind": p.ToolCall.Kind, "allowed": v.Allow, "reason": v.Reason})
	return answer(v, p.Options)
}

// summary is the agent's final answer: what it said after its last tool call, or everything when
// it said nothing after one.
func (d *driver) summary() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushParagraphs(true)
	final := d.segments[min(d.sinceTool, len(d.segments)):]
	if len(final) == 0 {
		final = d.segments
	}
	return clip(strings.Join(final, "\n\n"), maxSummary)
}

// setup is initialize and the session the prompt runs in: the one asked for, loaded, when the
// agent can load sessions, else a new one in the job's directory with its MCP servers.
type setup struct {
	sessionID string
	resumable bool // the session can be loaded again: a steer resumes it
	model     string
	agentInfo string
}

func (d *driver) start(ctx context.Context, c *conn) (setup, error) {
	var out setup
	step, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	var init initializeResult
	if err := c.call(step, methodInitialize, initializeParams{ProtocolVersion: ProtocolVersion,
		ClientInfo: &implementation{Name: "gator-runner", Version: version.Version}}, &init); err != nil {
		return out, fmt.Errorf("initialize: %w", err)
	}
	if init.ProtocolVersion != ProtocolVersion {
		return out, fmt.Errorf("the agent speaks ACP %d, this runner %d", init.ProtocolVersion, ProtocolVersion)
	}
	if init.AgentInfo != nil {
		out.agentInfo = strings.TrimSpace(init.AgentInfo.Name + " " + init.AgentInfo.Version)
	}
	servers := d.mcpServers(init.AgentCapabilities)
	var modes *modeState
	var models *modelsState
	if d.spec.SessionID != "" && init.AgentCapabilities.LoadSession {
		d.setLoading(true)
		var r loadSessionResult
		err := c.call(step, methodLoad, loadSessionParams{SessionID: d.spec.SessionID, CWD: d.spec.Dir, MCPServers: servers}, &r)
		d.setLoading(false)
		if err != nil {
			return out, fmt.Errorf("resume session %s: %w", d.spec.SessionID, err)
		}
		out.sessionID, modes, models = d.spec.SessionID, r.Modes, r.Models
	} else {
		var r newSessionResult
		if err := c.call(step, methodNewSession, newSessionParams{CWD: d.spec.Dir, MCPServers: servers}, &r); err != nil {
			return out, fmt.Errorf("new session: %w", err)
		}
		if r.SessionID == "" {
			return out, errors.New("new session: the agent gave no session id")
		}
		out.sessionID, modes, models = r.SessionID, r.Modes, r.Models
	}
	out.resumable = init.AgentCapabilities.LoadSession
	d.mu.Lock()
	d.sessionID = out.sessionID
	d.mu.Unlock()
	if d.spec.Access.ReadOnly {
		// A read-only job needs the agent's own read-only mode as well as the permission answers:
		// not every tool of an agent asks before it acts.
		if !modes.has(d.profile.ReadOnlyMode) {
			return out, fmt.Errorf("the agent offers no read-only mode (%q); a read-only job does not run on it", d.profile.ReadOnlyMode)
		}
		if err := c.call(step, methodSetMode, setModeParams{SessionID: out.sessionID, ModeID: d.profile.ReadOnlyMode}, nil); err != nil {
			return out, fmt.Errorf("set the read-only mode: %w", err)
		}
	}
	if models != nil {
		out.model = models.CurrentModelID
	}
	if m := d.spec.Model; m != "" {
		if models.has(m) && c.call(step, methodSetModel, setModelParams{SessionID: out.sessionID, ModelID: m}, nil) == nil {
			out.model = m
		} else {
			d.emit("warning", map[string]any{"message": "the agent does not offer model " + m + "; it runs its own default"})
		}
	}
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s.Name)
	}
	d.emit("session", map[string]any{"session_id": out.sessionID, "backend": d.agent, "agent": out.agentInfo,
		"model": out.model, "mcp_servers": names, "load_session": out.resumable, "experimental": d.profile.Experimental})
	return out, nil
}

func (d *driver) setLoading(v bool) {
	d.mu.Lock()
	d.loading = v
	d.mu.Unlock()
}

// mcpServers are the job's MCP servers as the session takes them, when the agent can reach a
// server over HTTP. One that cannot is told nothing of them: the job still has gator-cli.
func (d *driver) mcpServers(caps agentCapabilities) []mcpServer {
	out := []mcpServer{}
	if len(d.spec.MCP) == 0 {
		return out
	}
	if !caps.MCPCapabilities.HTTP {
		d.emit("warning", map[string]any{"message": "the agent cannot reach MCP servers over HTTP; Gator's tools are left out of its session"})
		return out
	}
	for _, s := range d.spec.MCP {
		h := []nameValue{}
		for k, v := range s.Headers {
			h = append(h, nameValue{Name: k, Value: v})
		}
		out = append(out, mcpServer{Type: "http", Name: s.Name, URL: s.URL, Headers: h})
	}
	return out
}

// prompt sends the job's prompt and waits for the end of the turn. When ctx ends first — a stop,
// a steer, a limit — the turn is cancelled (session/cancel) and the agent given grace to say so.
func (d *driver) prompt(ctx context.Context, c *conn, sessionID string, grace time.Duration) (promptResult, error) {
	type answer struct {
		r   promptResult
		err error
	}
	got := make(chan answer, 1)
	go func() {
		var r promptResult
		err := c.call(context.Background(), methodPrompt, promptParams{SessionID: sessionID,
			Prompt: []contentBlock{{Type: "text", Text: d.spec.Prompt}}}, &r)
		got <- answer{r, err}
	}()
	select {
	case a := <-got:
		return a.r, a.err
	case <-ctx.Done():
	}
	_ = c.notify(methodCancel, cancelParams{SessionID: sessionID})
	select {
	case a := <-got:
		return a.r, a.err
	case <-time.After(grace):
		return promptResult{StopReason: stopCancelled}, ctx.Err()
	case <-c.done:
		return promptResult{StopReason: stopCancelled}, errClosed
	}
}

// outcome reads the end of a run.
func (d *driver) outcome(runCtx context.Context, s setup, r promptResult, err error, started, finished time.Time) backend.Outcome {
	d.mu.Lock()
	calls, cost, known := d.toolCalls, d.costUSD, d.costKnown
	d.mu.Unlock()
	out := backend.Outcome{Model: s.model, ToolCalls: calls, Summary: d.summary()}
	if s.resumable {
		out.SessionID = s.sessionID // only a session the agent can load again is worth resuming
	}
	out.Usage = proto.Usage{Backend: d.agent, Model: s.model, DurationMS: finished.Sub(started).Milliseconds(),
		StartedAt: started, FinishedAt: finished, CostUSD: cost, CostEstimated: !known}
	if u := r.Usage; u != nil {
		out.Usage.InputTokens, out.Usage.OutputTokens = u.InputTokens, u.OutputTokens+u.ThoughtTokens
		out.Usage.CacheReadTokens, out.Usage.CacheWriteTokens = u.CachedReadTokens, u.CachedWriteTokens
	}
	cause := context.Cause(runCtx)
	switch {
	case errors.Is(cause, errToolLimit):
		out.Status, out.Reason = proto.StatusFailed, fmt.Sprintf("tool call limit (%d) reached", d.spec.MaxToolCalls)
	case errors.Is(cause, errCostLimit):
		out.Status, out.Reason = proto.StatusFailed, fmt.Sprintf("cost limit ($%.2f) reached: $%.2f", d.spec.MaxCostUSD, cost)
	case runCtx.Err() != nil:
		out.Interrupted, out.Cause, out.Reason = true, cause, cause.Error()
	case err != nil:
		out.Status, out.Reason = proto.StatusFailed, d.agent+": "+clip(err.Error(), 500)
	default:
		d.emit("result", map[string]any{"stop_reason": r.StopReason, "cost_usd": cost})
		switch r.StopReason {
		case stopEndTurn:
			out.Status = proto.StatusDone
		case stopCancelled:
			out.Status, out.Reason = proto.StatusFailed, d.agent+" cancelled the turn by itself"
		case stopMaxTokens, stopMaxTurns, stopRefusal:
			out.Status, out.Reason = proto.StatusFailed, d.agent+" stopped: "+r.StopReason
		default:
			out.Status, out.Reason = proto.StatusFailed, fmt.Sprintf("%s ended with stop reason %q", d.agent, r.StopReason)
		}
	}
	return out
}

func cut(s string) string {
	if len(s) > maxPayload {
		return s[:maxPayload] + fmt.Sprintf("… [%d bytes truncated]", len(s)-maxPayload)
	}
	return s
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// trimJSON keeps a tool input readable but bounded.
func trimJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return cut(string(raw))
	}
	return trimValue(v)
}

func trimValue(v any) any {
	switch x := v.(type) {
	case string:
		return cut(x)
	case map[string]any:
		for k, vv := range x {
			x[k] = trimValue(vv)
		}
	case []any:
		for i := range x {
			x[i] = trimValue(x[i])
		}
	}
	return v
}
