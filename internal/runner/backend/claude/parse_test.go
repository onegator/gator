package claude

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

type captured struct {
	typ     string
	payload map[string]any
}

func parseFile(t *testing.T, name string) (*Parser, []captured) {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := &Parser{}
	var evs []captured
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		_ = p.Line(sc.Bytes(), func(typ string, payload any) {
			m, _ := payload.(map[string]any)
			evs = append(evs, captured{typ, m})
		})
	}
	return p, evs
}

func types(evs []captured) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.typ)
	}
	return strings.Join(s, ",")
}

// Recorded with Claude Code 2.1.270: one Bash call, then "DONE".
func TestRecordedToolSuccess(t *testing.T) {
	p, evs := parseFile(t, "claude-2.1.270-tool-success.ndjson")
	if p.Result == nil || p.Result.Subtype != "success" || p.Result.IsError || p.Result.Text != "DONE" {
		t.Fatalf("result: %+v", p.Result)
	}
	if p.ToolCalls != 1 || p.BadLines != 0 {
		t.Fatalf("tool calls %d bad lines %d", p.ToolCalls, p.BadLines)
	}
	if p.Version != "2.1.270" || p.Model == "" || p.SessionID == "" {
		t.Fatalf("session: %q %q %q", p.Version, p.Model, p.SessionID)
	}
	if got := types(evs); got != "rate_limit,session,tool_call,tool_result,text,result" {
		t.Fatalf("event order: %s", got)
	}
	if evs[2].payload["name"] != "Bash" || evs[3].payload["content"] != "gator-fixture" || evs[3].payload["is_error"] != false {
		t.Fatalf("tool events: %+v %+v", evs[2].payload, evs[3].payload)
	}
	// Tokens must include every model in modelUsage, not only the top-level usage block.
	r := p.Result
	if r.Tokens.Input <= 34 || r.Tokens.CacheRead != 39662 || r.Tokens.CacheWrite != 24069 || r.Tokens.Output != 153 {
		t.Fatalf("tokens: %+v", r.Tokens)
	}
	if r.CostUSD <= 0 || !r.CostEstimated {
		t.Fatalf("cost %v estimated %v", r.CostUSD, r.CostEstimated)
	}
}

func TestRecordedMaxTurns(t *testing.T) {
	p, _ := parseFile(t, "claude-2.1.270-max-turns.ndjson")
	if p.Result == nil || p.Result.Subtype != "error_max_turns" || !p.Result.IsError || p.Result.TerminalReason != "max_turns" {
		t.Fatalf("result: %+v", p.Result)
	}
	if p.Result.Tokens.CacheWrite == 0 || p.Result.CostUSD <= 0 {
		t.Fatalf("an errored session still reports usage: %+v", p.Result)
	}
}

func TestTruncatedStreamHasNoResult(t *testing.T) {
	p, _ := parseFile(t, "synthetic-no-result.ndjson")
	if p.Result != nil {
		t.Fatal("a stream cut before the result line must not yield a result")
	}
}

func TestGarbageLinesCounted(t *testing.T) {
	p, _ := parseFile(t, "synthetic-garbage.ndjson")
	if p.BadLines != 1 || p.Result != nil || p.Model != "m" {
		t.Fatalf("bad lines %d result %v", p.BadLines, p.Result)
	}
}

func TestHooksAndThinkingAreNotForwarded(t *testing.T) {
	p := &Parser{}
	var got []string
	emit := func(typ string, _ any) { got = append(got, typ) }
	_ = p.Line([]byte(`{"type":"system","subtype":"hook_response","stdout":"secret local output"}`), emit)
	_ = p.Line([]byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"private"}]}}`), emit)
	_ = p.Line([]byte(`{"type":"future_thing","x":1}`), emit)
	if len(got) != 0 || p.Unknown != 1 {
		t.Fatalf("forwarded %v unknown %d", got, p.Unknown)
	}
}

func TestLongPayloadsAreTruncated(t *testing.T) {
	p := &Parser{MaxPayload: 10}
	var payload map[string]any
	_ = p.Line([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"Write","input":{"content":"0123456789ABCDEFGHIJ"}}]}}`),
		func(_ string, pl any) { payload, _ = pl.(map[string]any) })
	in := payload["input"].(map[string]any)["content"].(string)
	if !strings.HasPrefix(in, "0123456789…") {
		t.Fatalf("not truncated: %q", in)
	}
}
