//go:build unix

package acp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/backend/replay"
)

// Each supported agent's recordings replay through the real client (a process, the protocol, the
// permission answers) into the job events and the outcome their golden files say.
func TestRecordingsReplay(t *testing.T) {
	cases := map[string]backend.Spec{
		"codex/synthetic-work-tools.json":       {Prompt: "do the task", Model: "gpt-5"},
		"codex/synthetic-chat-permissions.json": {Prompt: "what is this?", Access: backend.Access{ReadOnly: true, Commands: []string{"gator-cli"}}},
		"codex/synthetic-tool-limit.json":       {Prompt: "go", MaxToolCalls: 2},
		"codex/synthetic-mode-left.json":        {Prompt: "look", Access: backend.Access{ReadOnly: true}},
		"gemini/synthetic-auth-required.json":   {Prompt: "hi"},
		"gemini/synthetic-no-load-no-http.json": {Prompt: "hi"},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			b, spec, _ := fake(t, name, spec)
			rec := &replay.Recorder{}
			out := b.Run(context.Background(), spec, rec.Emit)
			golden := filepath.Join("testdata", "golden", strings.ReplaceAll(strings.TrimSuffix(name, ".json"), "/", "-")+".golden.json")
			replay.Compare(t, golden, rec.Events(), map[string]any{"status": out.Status, "reason": out.Reason,
				"session_id": out.SessionID, "tool_calls": out.ToolCalls, "summary": out.Summary, "cost_usd": out.Usage.CostUSD,
				"cost_estimated": out.Usage.CostEstimated, "input_tokens": out.Usage.InputTokens, "output_tokens": out.Usage.OutputTokens})
		})
	}
}
