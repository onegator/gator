//go:build unix

package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
)

type event struct {
	Type    string
	Payload map[string]any
}

type recorder struct {
	mu     sync.Mutex
	events []event
}

func (r *recorder) emit(typ string, payload any) {
	b, _ := json.Marshal(payload)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	r.mu.Lock()
	r.events = append(r.events, event{typ, m})
	r.mu.Unlock()
}

func (r *recorder) of(typ string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, e := range r.events {
		if e.Type == typ {
			out = append(out, e.Payload)
		}
	}
	return out
}

// fake runs the test binary as an ACP agent playing testdata/<name>.
func fake(t *testing.T, name string, spec backend.Spec) (Backend, backend.Spec, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "agent.log")
	agent := strings.SplitN(name, "/", 2)[0]
	b := New(Agent{Name: agent, Command: []string{self}}, nil, nil)
	b.GracePeriod = 2 * time.Second
	if spec.Dir == "" {
		spec.Dir = t.TempDir()
	}
	spec.Env = append(spec.Env, "ACPFAKE_SCRIPT="+script, "ACPFAKE_LOG="+logPath)
	return b, spec, logPath
}

type logged struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Answer json.RawMessage `json:"answer"`
}

func readLog(t *testing.T, path string) []logged {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []logged
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l logged
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			out = append(out, l)
		}
	}
	return out
}

func methods(l []logged) []string {
	var out []string
	for _, x := range l {
		if x.Method != "" {
			out = append(out, x.Method)
		}
	}
	return out
}

func find(l []logged, method string) json.RawMessage {
	for _, x := range l {
		if x.Method == method {
			return x.Params
		}
	}
	return nil
}

func answers(l []logged) []string {
	var out []string
	for _, x := range l {
		if len(x.Answer) > 0 {
			var r permissionResult
			_ = json.Unmarshal(x.Answer, &r)
			out = append(out, r.Outcome.Outcome+":"+r.Outcome.OptionID)
		}
	}
	return out
}

func TestWorkSessionStreamsWordsToolsAndUsage(t *testing.T) {
	rec := &recorder{}
	b, spec, logPath := fake(t, "codex/synthetic-work-tools.json", backend.Spec{Prompt: "do the task", Model: "gpt-5",
		MCP: []backend.MCPServer{{Name: "gator", URL: "http://gator.test/mcp", Headers: map[string]string{"Authorization": "Bearer job-token"}}}})
	out := b.Run(context.Background(), spec, rec.emit)
	if out.Status != proto.StatusDone {
		t.Fatalf("status %q: %s", out.Status, out.Reason)
	}
	if b.Name() != "acp:codex" || out.Usage.Backend != "acp:codex" {
		t.Errorf("backend name %q / %q", b.Name(), out.Usage.Backend)
	}
	if out.SessionID != "019a5f3c-codex-session" {
		t.Errorf("session %q: a loadable session is kept for a steer", out.SessionID)
	}
	if out.ToolCalls != 2 {
		t.Errorf("tool calls %d", out.ToolCalls)
	}
	var texts []string
	for _, p := range rec.of("text") {
		texts = append(texts, p["text"].(string))
	}
	// Paragraphs, whole, in order; the thought is not forwarded.
	want := []string{"I'll read the README first.", "Done.", "The answer is 42."}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Errorf("texts %q, want %q", texts, want)
	}
	if out.Summary != "Done.\n\nThe answer is 42." {
		t.Errorf("summary %q: the words after the last tool call", out.Summary)
	}
	if calls := rec.of("tool_call"); len(calls) != 2 || calls[0]["name"] != "Read README.md" || calls[1]["kind"] != "edit" {
		t.Errorf("tool calls %v", calls)
	}
	if res := rec.of("tool_result"); len(res) != 2 || res[0]["content"] != "# Project\nhello" || res[1]["content"] != "[diff main.go]" {
		t.Errorf("tool results %v", res)
	}
	if len(rec.of("plan")) != 1 {
		t.Error("the plan is an event")
	}
	if out.Usage.CostUSD != 0.0123 || out.Usage.CostEstimated {
		t.Errorf("cost %v estimated %v: the agent reported it", out.Usage.CostUSD, out.Usage.CostEstimated)
	}
	if out.Usage.InputTokens != 12000 || out.Usage.OutputTokens != 400 || out.Usage.CacheReadTokens != 6000 {
		t.Errorf("tokens %+v", out.Usage)
	}
	if out.Model != "gpt-5" {
		t.Errorf("model %q: the job's model is set when the agent offers it", out.Model)
	}
	l := readLog(t, logPath)
	got := strings.Join(methods(l), ",")
	if got != "initialize,session/new,session/set_model,session/prompt" {
		t.Errorf("client sent %s (a work job sets no read-only mode)", got)
	}
	// Full access answers yes, once — never "always".
	if a := answers(l); strings.Join(a, ",") != "selected:approved,selected:approved" {
		t.Errorf("permission answers %v", a)
	}
	var ns newSessionParams
	_ = json.Unmarshal(find(l, methodNewSession), &ns)
	if ns.CWD != spec.Dir || len(ns.MCPServers) != 1 || ns.MCPServers[0].Type != "http" || ns.MCPServers[0].Headers[0].Value != "Bearer job-token" {
		t.Errorf("session/new %s", find(l, methodNewSession))
	}
	var init initializeParams
	_ = json.Unmarshal(find(l, methodInitialize), &init)
	if init.ClientCapabilities.FS.WriteTextFile || init.ClientCapabilities.Terminal {
		t.Error("the client offers the agent no file system and no terminal")
	}
}

func TestReadOnlyJobAnswersPermissionsByTier(t *testing.T) {
	rec := &recorder{}
	b, spec, logPath := fake(t, "codex/synthetic-chat-permissions.json", backend.Spec{Prompt: "what is this?",
		Access: backend.Access{ReadOnly: true, Commands: []string{"gator-cli"}},
		MCP:    []backend.MCPServer{{Name: "gator", URL: "http://gator.test/mcp"}}})
	out := b.Run(context.Background(), spec, rec.emit)
	if out.Status != proto.StatusDone {
		t.Fatalf("status %q: %s", out.Status, out.Reason)
	}
	l := readLog(t, logPath)
	want := []string{
		"selected:yes", // read
		"selected:no",  // edit
		"selected:no",  // rm -rf through bash
		"selected:yes", // gator-cli through bash
		"selected:no",  // gator-cli chained with another command
		"selected:yes", // the job's own MCP server
		"selected:no",  // another tool the agent does not name as reading
		"cancelled:",   // delete, and the agent offered no way to say no
	}
	if got := answers(l); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("answers\n got %v\nwant %v", got, want)
	}
	if !strings.Contains(strings.Join(methods(l), ","), "session/set_mode") {
		t.Error("a read-only job sets the agent's read-only mode where it has one")
	}
	for _, x := range l {
		if x.Method == methodSetMode && !strings.Contains(string(x.Params), `"read-only"`) {
			t.Errorf("mode %s", x.Params)
		}
	}
	if perms := rec.of("permission"); len(perms) != 8 || perms[1]["allowed"] != false {
		t.Errorf("permission events %v", perms)
	}
}

// A read-only job on a session that does not offer the profile's read-only mode does not run.
func TestReadOnlyNeedsTheReadOnlyMode(t *testing.T) {
	b, spec, logPath := fake(t, "codex/synthetic-steer.json", backend.Spec{Prompt: "look", Access: backend.Access{ReadOnly: true}})
	out := b.Run(context.Background(), spec, (&recorder{}).emit)
	if out.Status != proto.StatusFailed || !strings.Contains(out.Reason, "read-only mode") {
		t.Fatalf("outcome %q %q", out.Status, out.Reason)
	}
	if strings.Contains(strings.Join(methods(readLog(t, logPath)), ","), methodPrompt) {
		t.Error("the prompt was sent")
	}
}

func TestSteerCancelsAndResumesTheLoadedSession(t *testing.T) {
	rec := &recorder{}
	b, spec, logPath := fake(t, "codex/synthetic-steer.json", backend.Spec{Prompt: "start"})
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		for len(rec.of("text")) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		cancel(context.Canceled)
	}()
	out := b.Run(ctx, spec, rec.emit)
	if !out.Interrupted || out.SessionID != "sess-steer" {
		t.Fatalf("outcome %+v: a steer interrupts and keeps the session", out)
	}
	if !strings.Contains(strings.Join(methods(readLog(t, logPath)), ","), "session/cancel") {
		t.Error("an interrupted turn is cancelled with session/cancel")
	}

	// The next run resumes it: the same script, turned to its second prompt by a fresh process,
	// which only works because the session is loaded rather than made anew.
	rec2 := &recorder{}
	b2, spec2, log2 := fake(t, "codex/synthetic-steer.json", backend.Spec{Prompt: "the correction", SessionID: out.SessionID})
	out2 := b2.Run(context.Background(), spec2, rec2.emit)
	l := readLog(t, log2)
	if got := strings.Join(methods(l), ","); !strings.HasPrefix(got, "initialize,session/load,session/prompt") {
		t.Errorf("resume sent %s", got)
	}
	// What the agent replays while loading is the session's past, not this run's words or tools.
	for _, p := range rec2.of("text") {
		if strings.Contains(p["text"].(string), "replayed") {
			t.Errorf("replayed history emitted: %v", p)
		}
	}
	if len(rec2.of("tool_call")) != 0 {
		t.Error("replayed tool calls are not this run's")
	}
	if out2.Status != proto.StatusDone || out2.Summary != "Corrected, as asked." {
		t.Errorf("resumed outcome %q %q", out2.Status, out2.Summary)
	}
}

func TestToolLimitStopsTheRun(t *testing.T) {
	b, spec, _ := fake(t, "codex/synthetic-tool-limit.json", backend.Spec{Prompt: "go", MaxToolCalls: 2})
	out := b.Run(context.Background(), spec, (&recorder{}).emit)
	if out.Status != proto.StatusFailed || !strings.Contains(out.Reason, "tool call limit (2)") {
		t.Fatalf("outcome %q %q", out.Status, out.Reason)
	}
	if out.SessionID != "" {
		t.Error("a session the agent cannot load is not offered for resuming")
	}
}

func TestAgentWithoutHTTPMCPOrLoadingStillRuns(t *testing.T) {
	rec := &recorder{}
	b, spec, logPath := fake(t, "gemini/synthetic-no-load-no-http.json", backend.Spec{Prompt: "hi",
		MCP: []backend.MCPServer{{Name: "gator", URL: "http://gator.test/mcp"}}})
	out := b.Run(context.Background(), spec, rec.emit)
	if out.Status != proto.StatusFailed || !strings.Contains(out.Reason, "max_turn_requests") {
		t.Errorf("outcome %q %q", out.Status, out.Reason)
	}
	if out.Summary != "Hello from Gemini." {
		t.Errorf("summary %q: another session's words are not this run's", out.Summary)
	}
	if len(rec.of("warning")) == 0 {
		t.Error("leaving Gator's MCP servers out is said")
	}
	l := readLog(t, logPath)
	var ns newSessionParams
	_ = json.Unmarshal(find(l, methodNewSession), &ns)
	if len(ns.MCPServers) != 0 {
		t.Errorf("servers %v given to an agent that cannot reach them", ns.MCPServers)
	}
	if out.Usage.CostEstimated != true || out.Usage.CostUSD != 0 {
		t.Errorf("usage %+v: no cost reported is an estimate", out.Usage)
	}
}

func TestAuthRequiredAndCrashFail(t *testing.T) {
	b, spec, _ := fake(t, "gemini/synthetic-auth-required.json", backend.Spec{Prompt: "hi"})
	out := b.Run(context.Background(), spec, (&recorder{}).emit)
	if out.Status != proto.StatusFailed || !strings.Contains(out.Reason, "Authentication required") {
		t.Errorf("outcome %q %q", out.Status, out.Reason)
	}
	b, spec, _ = fake(t, "gemini/synthetic-crash.json", backend.Spec{Prompt: "hi"})
	out = b.Run(context.Background(), spec, (&recorder{}).emit)
	if out.Status != proto.StatusFailed || !strings.Contains(out.Reason, "could not load settings.json") {
		t.Errorf("outcome %q %q", out.Status, out.Reason)
	}
}

func TestEnvironmentIsFiltered(t *testing.T) {
	t.Setenv("GATOR_RUNNER_TOKEN", "gtr_runner_secret")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("SOMETHING_ELSE", "x")
	b, spec, logPath := fake(t, "codex/synthetic-work-tools.json", backend.Spec{Prompt: "go", Env: []string{"GATOR_JOB_TOKEN=job"}})
	b.Run(context.Background(), spec, (&recorder{}).emit)
	var env []string
	for _, x := range readLogRaw(t, logPath) {
		if e, ok := x["env"]; ok {
			_ = json.Unmarshal(e, &env)
		}
	}
	got := strings.Join(env, "\n")
	for _, want := range []string{"OPENAI_API_KEY=sk-test", "GATOR_JOB_TOKEN=job"} {
		if !strings.Contains(got, want) {
			t.Errorf("the agent lacks %s", want)
		}
	}
	for _, never := range []string{"GATOR_RUNNER_TOKEN", "SOMETHING_ELSE"} {
		if strings.Contains(got, never) {
			t.Errorf("the agent got %s", never)
		}
	}
}

func readLogRaw(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]json.RawMessage
	for line := range strings.SplitSeq(string(b), "\n") {
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func TestAuthState(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{}
	b := New(Agent{Name: "codex", Command: []string{"/bin/sh"}}, nil, nil)
	b.Getenv = func(k string) string { return env[k] }
	b.Home = func() (string, error) { return home, nil }
	if s := b.AuthState(); s != "missing" {
		t.Errorf("no login: %q", s)
	}
	env["CODEX_HOME"], env["OPENAI_BASE_URL"] = "/x", "https://proxy"
	if s := b.AuthState(); s != "missing" {
		t.Errorf("configuration is not a login: %q", s)
	}
	env["OPENAI_API_KEY"] = "sk"
	if s := b.AuthState(); s != "ok" {
		t.Errorf("api key: %q", s)
	}
	delete(env, "OPENAI_API_KEY")
	_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600)
	if s := b.AuthState(); s != "ok" {
		t.Errorf("stored login: %q", s)
	}
	b.Agent.Command = []string{"/no/such/agent-acp"}
	if s := b.AuthState(); s != "no_cli" {
		t.Errorf("missing command: %q", s)
	}
	x := New(Agent{Name: "someday", Command: []string{"/bin/sh"}}, nil, nil)
	if s := x.AuthState(); s != "unknown" || !x.Profile.Experimental {
		t.Errorf("an agent without a profile: %q experimental %v", s, x.Profile.Experimental)
	}
	codex, gemini := New(Agent{Name: "codex"}, nil, nil), New(Agent{Name: "gemini"}, nil, nil)
	if codex.Supports(backend.Access{NoTools: true}) || !codex.Supports(backend.Access{ReadOnly: true}) || !codex.Supports(backend.Access{}) {
		t.Error("codex keeps read-only jobs with its read-only mode, and never jobs without tools")
	}
	if gemini.Supports(backend.Access{ReadOnly: true}) || !gemini.Supports(backend.Access{}) {
		t.Error("an agent without a read-only mode takes only work")
	}
}

func TestParseAgents(t *testing.T) {
	got, err := ParseAgents("codex:/usr/local/bin/codex-acp, gemini:/usr/local/bin/gemini --experimental-acp")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].BackendName() != "acp:gemini" || strings.Join(got[1].Command, " ") != "/usr/local/bin/gemini --experimental-acp" {
		t.Errorf("%+v", got)
	}
	for _, bad := range []string{"codex", "Codex:/x", "codex:", "codex:/a,codex:/b"} {
		if _, err := ParseAgents(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got, err := ParseAgents(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
}

// An agent that switches itself out of its read-only mode in a read-only job is stopped, and the
// job fails.
func TestLeavingTheReadOnlyModeEndsTheRun(t *testing.T) {
	b, spec, logPath := fake(t, "codex/synthetic-mode-left.json", backend.Spec{Prompt: "look", Access: backend.Access{ReadOnly: true}})
	out := b.Run(context.Background(), spec, (&recorder{}).emit)
	if out.Status != proto.StatusFailed || !strings.Contains(out.Reason, "left its read-only mode") {
		t.Fatalf("outcome %q %q", out.Status, out.Reason)
	}
	if !strings.Contains(strings.Join(methods(readLog(t, logPath)), ","), methodCancel) {
		t.Error("the turn was not cancelled")
	}
}

// What a run keeps of a runaway answer is bounded.
func TestAgentWordsAreBounded(t *testing.T) {
	d := newDriver("acp:x", Profile{}, backend.Spec{}, func(string, any) {}, func(error) {})
	chunk, _ := json.Marshal(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": strings.Repeat("x", 32<<10)}})
	for range 200 {
		d.update(updateParams{Update: chunk})
	}
	if d.pending.Len() > maxPending || d.kept > maxKept+maxPending {
		t.Errorf("pending %d kept %d", d.pending.Len(), d.kept)
	}
	if s := d.summary(); len(s) > maxSummary+10 {
		t.Errorf("summary %d", len(s))
	}
}
