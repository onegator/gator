package executor

import (
	"encoding/json"
	"strings"

	"github.com/onegator/gator/internal/proto"
)

const digestFence = "```gator-digest"

// DigestInstruction asks every job to end with a machine-readable account of the session.
const DigestInstruction = "End your final message with a fenced code block tagged gator-digest holding JSON like " +
	`{"changes":[],"decisions":[],"rejected":[],"left":[]}` +
	": what you changed, the decisions you made and why, approaches you rejected, and what is left. " +
	"One short sentence per item, empty lists where nothing applies. Gator removes the block before people read your message."

// DigestPrompt resumes a session that ended without its digest.
const DigestPrompt = "Reply with only the gator-digest block for the work in this session: a fenced code block tagged gator-digest " +
	`holding JSON {"changes":[],"decisions":[],"rejected":[],"left":[]}` +
	", one short sentence per item. Nothing else."

// ExtractDigest finds the last gator-digest block in an answer, decodes it and returns the
// answer without it. An absent or malformed block yields nil and the answer unchanged.
func ExtractDigest(answer string) (*proto.Digest, string) {
	start := strings.LastIndex(answer, digestFence)
	if start < 0 {
		return nil, answer
	}
	body := answer[start+len(digestFence):]
	end := strings.Index(body, "```")
	if end < 0 {
		return nil, answer
	}
	var d proto.Digest
	if err := json.Unmarshal([]byte(strings.TrimSpace(body[:end])), &d); err != nil {
		return nil, answer
	}
	d = normalize(d)
	rest := strings.TrimSpace(answer[:start] + body[end+3:])
	return &d, rest
}

func normalize(d proto.Digest) proto.Digest {
	clean := func(in []string) []string {
		out := []string{}
		for _, s := range in {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if len(s) > 500 {
				s = s[:500] + "…"
			}
			out = append(out, s)
			if len(out) == 20 {
				break
			}
		}
		return out
	}
	return proto.Digest{Changes: clean(d.Changes), Decisions: clean(d.Decisions), Rejected: clean(d.Rejected), Left: clean(d.Left)}
}
