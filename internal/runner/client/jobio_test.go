package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/proto"
)

// A masked job sends no secret over any of its channels: events, turn endings, checkpoints and
// requests for a person's decision.
func TestMaskedCoversEveryOutgoingChannel(t *testing.T) {
	const secret = "sk-live-0123456789"
	var sent []string
	io := JobIO{
		Emit:       func(_ string, p any) { b, _ := json.Marshal(p); sent = append(sent, string(b)) },
		TurnEnding: func(te proto.TurnEnding) []proto.Objection { sent = append(sent, te.Summary); return nil },
		Checkpoint: func(cp proto.Checkpoint) { sent = append(sent, string(cp.Diff), string(cp.Whole), cp.Files[0].Path) },
		Request: func(_ context.Context, r proto.RuntimeRequest) proto.RuntimeDecision {
			sent = append(sent, string(r.Input))
			return proto.RuntimeDecision{}
		},
	}.Masked(func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") })
	io.Emit("text", map[string]string{"text": secret})
	io.TurnEnding(proto.TurnEnding{Summary: secret})
	io.Checkpoint(proto.Checkpoint{Diff: []byte(secret), Whole: []byte(secret), Files: []proto.FileChange{{Path: secret}}})
	io.Request(context.Background(), proto.RuntimeRequest{Input: []byte(`{"command":"curl -H ` + secret + `"}`)})
	for _, s := range sent {
		if strings.Contains(s, secret) {
			t.Fatalf("a secret went out: %q", s)
		}
	}
	if len(sent) != 6 {
		t.Fatalf("sent %d", len(sent))
	}
}
