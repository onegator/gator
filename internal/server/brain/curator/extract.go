// Package curator reads what a team settled in its conversations and puts it in front of a person
// as Brain proposals: after a thread goes quiet, or when its task closes, a curation job (the
// @curator, a cheap model with no tools) reads the thread and writes down what was agreed; what
// it writes becomes proposals, and a card in the thread lists the day's proposals for a person
// to approve or turn down. Reading the conversation into an instruction and the answer into
// candidates (Extract) is apart from storing the proposals (the Brain's own Service).
package curator

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/conversations"
)

// Fence is the block a curator answers in.
const Fence = "```gator-brain"

// Kinds a conversation can settle.
var Kinds = []string{brain.KindDecision, brain.KindLesson, brain.KindRunbook, brain.KindFact, brain.KindGlossary, brain.KindConvention}

// Limits of one curation.
const (
	// MaxCandidates is how much one curation proposes: a thread does not fill an inbox.
	MaxCandidates = 5
	// maxInput bounds the transcript one curation reads; the newest messages are kept.
	maxInput = 40000
	// maxContent bounds one candidate.
	maxContent = 4000
)

// Candidate is one thing a conversation settled, as the curator wrote it down.
type Candidate struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Content string `json:"content"`
	// Quote is the words that settled it, and Seq the message they were said in.
	Quote string `json:"quote"`
	Seq   int64  `json:"seq"`
}

// Extract turns a conversation into the curator's instruction and the curator's answer into
// candidates. It reads and writes nothing.
type Extract struct{}

// Instruction is what the curator reads: the task, the conversation (members' words, numbered;
// words from outside are left out, as they are of every summary) and how to answer. since is the
// last seq an earlier curation covered: what was settled before it is already proposed.
func (Extract) Instruction(p conversations.Pack, since int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Read this conversation (%s) and write down what the people in it settled", p.Where)
	if since > 0 {
		fmt.Fprintf(&b, " after message #%d", since)
	}
	b.WriteString(": decisions with their reasons, lessons, runbook steps, facts the team now relies on. " +
		"Only what was agreed, not what was merely suggested, asked or later taken back. ")
	fmt.Fprintf(&b, "At most %d, the most lasting first; none is a fine answer.\n\n", MaxCandidates)
	b.WriteString("Answer with only a fenced block tagged gator-brain holding JSON like\n")
	b.WriteString(Fence + "\n" + `{"proposals":[{"kind":"decision","title":"Sessions use PASETO","content":"Session tokens are PASETO v4 local, not JWT, because …","quote":"the words that settled it","seq":12}]}` + "\n```\n")
	fmt.Fprintf(&b, "kind is one of %s; seq is the number of the message the quote is from.\n\n", strings.Join(Kinds, ", "))
	b.WriteString("The conversation, oldest first:\n\n")
	b.WriteString(transcript(append(append([]conversations.Said{}, p.Older...), p.Recent...)))
	return b.String()
}

// transcript is the members' words, numbered, the newest kept when there is too much.
func transcript(said []conversations.Said) string {
	var lines []string
	size, outside := 0, 0
	for i := len(said) - 1; i >= 0; i-- {
		s := said[i]
		if s.Untrusted {
			outside++ // what comes from outside never passes through a curation into the Brain
			continue
		}
		line := fmt.Sprintf("#%d %s: %s", s.Seq, s.Who, strings.TrimSpace(s.Body))
		if size+len(line) > maxInput {
			lines = append(lines, "… [older messages left out]")
			break
		}
		size += len(line)
		lines = append(lines, line)
	}
	slices.Reverse(lines)
	out := strings.Join(lines, "\n\n")
	if outside > 0 {
		out += fmt.Sprintf("\n\n(%d messages from outside the workspace are left out.)", outside)
	}
	return out
}

// Candidates reads the curator's answer: the last gator-brain block, the kinds it may propose,
// each with a title and content, at most MaxCandidates. Anything else is no answer.
func (Extract) Candidates(answer string) []Candidate {
	_, body, ok := strings.Cut(answer[lastFence(answer):], Fence)
	if !ok {
		return nil
	}
	block, _, ok := strings.Cut(body, "```")
	if !ok {
		return nil
	}
	var parsed struct {
		Proposals []Candidate `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(block)), &parsed); err != nil {
		return nil
	}
	var out []Candidate
	for _, c := range parsed.Proposals {
		c.Kind, c.Title, c.Content, c.Quote = strings.TrimSpace(c.Kind), strings.TrimSpace(c.Title), strings.TrimSpace(c.Content), strings.TrimSpace(c.Quote)
		if !slices.Contains(Kinds, c.Kind) || c.Title == "" || c.Content == "" {
			continue
		}
		if len(c.Content) > maxContent {
			c.Content = strings.ToValidUTF8(c.Content[:maxContent], "") + "…"
		}
		out = append(out, c)
		if len(out) == MaxCandidates {
			break
		}
	}
	return out
}

func lastFence(s string) int {
	if i := strings.LastIndex(s, Fence); i >= 0 {
		return i
	}
	return 0
}
