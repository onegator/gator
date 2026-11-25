package claude

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/runner/backend/replay"
)

// Every recording in testdata replays through the real parser into the events and result its
// golden file says: a backend without recordings is not a supported one.
func TestRecordingsReplay(t *testing.T) {
	files, err := filepath.Glob("testdata/*.ndjson")
	if err != nil || len(files) == 0 {
		t.Fatalf("no recordings: %v", err)
	}
	for _, path := range files {
		name := filepath.Base(path)
		if strings.Contains(name, "-session-") {
			continue // the session recordings are scripts the fake CLI plays (session_test.go)
		}
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			p := &Parser{}
			rec := &replay.Recorder{}
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<20), 32<<20)
			for sc.Scan() {
				_ = p.Line(sc.Bytes(), rec.Emit)
			}
			replay.Compare(t, filepath.Join("testdata", "golden", strings.TrimSuffix(name, ".ndjson")+".golden.json"), rec.Events(), parserOutcome(p))
		})
	}
}

// parserOutcome is what a replay came to, without the run's wall time.
func parserOutcome(p *Parser) map[string]any {
	out := map[string]any{"session_id": p.SessionID, "model": p.Model, "version": p.Version, "tool_calls": p.ToolCalls,
		"bad_lines": p.BadLines, "unknown": p.Unknown}
	if r := p.Result; r != nil {
		out["result"] = map[string]any{"subtype": r.Subtype, "is_error": r.IsError, "text_bytes": len(r.Text), "cost_usd": r.CostUSD,
			"cost_estimated": r.CostEstimated, "tokens": r.Tokens, "terminal_reason": r.TerminalReason}
	}
	return out
}
