package client

import (
	"context"
	"encoding/json"

	"github.com/onegator/gator/internal/proto"
)

// Masked is io with mask applied to everything the job sends while it runs: its events, what it
// says asking to end its turn, its checkpoints (diffs and paths) and what it asks a person (the
// tool's input). It is the one place a job's outgoing words are redacted on the runner.
func (io JobIO) Masked(mask func(string) string) JobIO {
	if mask == nil {
		return io
	}
	if emit := io.Emit; emit != nil {
		io.Emit = func(typ string, payload any) {
			b, err := json.Marshal(payload)
			if err != nil {
				emit(typ, payload)
				return
			}
			emit(typ, json.RawMessage(mask(string(b))))
		}
	}
	if ask := io.TurnEnding; ask != nil {
		io.TurnEnding = func(te proto.TurnEnding) []proto.Objection {
			te.Summary = mask(te.Summary)
			return ask(te)
		}
	}
	if send := io.Checkpoint; send != nil {
		io.Checkpoint = func(cp proto.Checkpoint) {
			cp.Diff, cp.Whole = []byte(mask(string(cp.Diff))), []byte(mask(string(cp.Whole)))
			for _, files := range [][]proto.FileChange{cp.Files, cp.WholeFiles} {
				for i := range files {
					files[i].Path = mask(files[i].Path)
				}
			}
			send(cp)
		}
	}
	if request := io.Request; request != nil {
		io.Request = func(ctx context.Context, r proto.RuntimeRequest) proto.RuntimeDecision {
			r.Input = json.RawMessage(mask(string(r.Input)))
			return request(ctx, r)
		}
	}
	return io
}
