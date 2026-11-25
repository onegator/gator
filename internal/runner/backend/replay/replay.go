// Package replay plays a recorded agent session through a backend's real parser and compares the
// job events it emits, and what the run came to, with a golden file kept next to the recording.
// A backend is supported once its recordings replay: a change to a parser that changes what a
// job reports shows up here as a diff of the golden file, not in production.
//
// The golden files are written by the tests themselves: run them with GATOR_UPDATE_GOLDEN=1 to
// rewrite every golden file a test compares with, and read the diff before committing it.
package replay

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// UpdateEnv names the variable that rewrites golden files instead of comparing with them.
const UpdateEnv = "GATOR_UPDATE_GOLDEN"

// Event is one job event as a golden file keeps it.
type Event struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Recorder collects what a backend emits. Its Emit is a backend.Emit.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

// Emit records one event, its payload as JSON.
func (r *Recorder) Emit(typ string, payload any) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			b, _ = json.Marshal(map[string]string{"error": err.Error()})
		}
		raw = b
	}
	r.mu.Lock()
	r.events = append(r.events, Event{Type: typ, Payload: raw})
	r.mu.Unlock()
}

// Events are what was recorded, in order.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// Types are the recorded events' types, in order, comma-separated.
func (r *Recorder) Types() string {
	var out []string
	for _, e := range r.Events() {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

// Golden is what a replay is compared with: the events, and a summary of how the run ended that
// the caller makes of its outcome (without anything that differs run to run: durations, times).
type Golden struct {
	Events  []Event `json:"events"`
	Outcome any     `json:"outcome"`
}

// Compare checks a replay against the golden file at path, or writes it when UpdateEnv is set. A
// missing golden file fails the test: a recording nobody looked at the output of proves nothing.
func Compare(t testing.TB, path string, events []Event, outcome any) {
	t.Helper()
	got, err := encode(Golden{Events: events, Outcome: outcome})
	if err != nil {
		t.Fatalf("replay: encoding %s: %v", path, err)
	}
	if os.Getenv(UpdateEnv) != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("replay: %v (run the test with %s=1 to write it, then read it)", err, UpdateEnv)
	}
	if !bytes.Equal(normalise(want), normalise(got)) {
		t.Fatalf("replay of %s differs from its golden file (rerun with %s=1 to accept):\n--- want\n%s\n--- got\n%s",
			path, UpdateEnv, want, got)
	}
}

// encode writes the golden file: indented, one event per line, so a diff of it reads.
func encode(g Golden) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("{\n  \"events\": [")
	for i, e := range g.Events {
		line, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n    ")
		b.Write(line)
	}
	if len(g.Events) > 0 {
		b.WriteString("\n  ")
	}
	b.WriteString("],\n  \"outcome\": ")
	out, err := json.MarshalIndent(g.Outcome, "  ", "  ")
	if err != nil {
		return nil, err
	}
	b.Write(out)
	b.WriteString("\n}\n")
	return b.Bytes(), nil
}

// normalise compares two golden files by what they say, not how they are spaced.
func normalise(b []byte) []byte {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	out, _ := json.Marshal(v)
	return out
}
