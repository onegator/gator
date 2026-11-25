package executor

import (
	"fmt"
	"strings"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/workspace"
)

// prompter writes the opening prompt of one kind of job.
type prompter func(job proto.Job, ws *workspace.Workspace) string

// prompts are the opening prompts by kind of job; a kind without one is a phase job.
var prompts = map[string]prompter{
	proto.KindPhase:   func(job proto.Job, ws *workspace.Workspace) string { return Prompt(job, ws.HasRepo()) },
	proto.KindReply:   ReplyPrompt,
	proto.KindSummary: SummaryPrompt,
}

func promptFor(job proto.Job, ws *workspace.Workspace) string {
	if p, ok := prompts[job.KindOrPhase()]; ok {
		return p(job, ws)
	}
	return Prompt(job, ws.HasRepo())
}

// ConversationTools is what a reply's prompt adds to AgentTools: the conversation is a place the
// agent can speak in, read further back in, and propose work from.
const ConversationTools = `In this conversation:

- ` + "`gator-cli say --file -`" + ` — say something more in the conversation, beside your answer; mention @agent to ask another agent.
- ` + "`gator-cli thread show [--limit N]`" + ` — read further back than your prompt carries.
- ` + "`gator-cli task create --kind bug|feature --title \"...\" --file -`" + ` — propose a task; a person confirms it.`

// ReplyPrompt opens a reply: the agent's own prompt, who asks it what and where, the
// conversation (its background, then its latest messages), what the task already knows, what
// the agent may touch, and the ask again at the end, where it is read last.
func ReplyPrompt(job proto.Job, ws *workspace.Workspace) string {
	var b strings.Builder
	if guide := strings.TrimSpace(job.Guide); guide != "" {
		b.WriteString(guide)
	} else {
		fmt.Fprintf(&b, "You are %s, an agent of this project's team.", who(job))
	}
	b.WriteString("\n\n---\n\n")
	turn := job.Turn
	if turn == nil {
		turn = &proto.Turn{Ask: "You are asked: " + job.Instruction}
	}
	b.WriteString(strings.TrimSpace(turn.Ask))
	b.WriteString("\n")
	for _, doc := range turn.Conversation {
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", doc.Title, strings.TrimSpace(doc.Body))
	}
	if t := strings.TrimSpace(job.TaskTitle); t != "" {
		fmt.Fprintf(&b, "\nThis conversation belongs to the task %q.\n", t)
	}
	for _, doc := range job.Context {
		fmt.Fprintf(&b, "\n## Context: %s\n\n%s\n", doc.Title, strings.TrimSpace(doc.Body))
	}
	b.WriteString("\n")
	switch {
	case ws.ReadOnly && ws.HasRepo():
		b.WriteString("The current directory is a read-only checkout of the project. You have no tools that write: read, search and answer. To change code, say so; a person decides whether an agent may work on it.")
	case ws.ReadOnly:
		b.WriteString("You have no tools that write: read, search and answer.")
	case ws.HasRepo():
		b.WriteString("The current directory is a git checkout of the project on its own branch. Commit what you change with clear messages; do not push, the runner does that.")
	default:
		b.WriteString("There is no repository for this conversation.")
	}
	if job.AgentToken != "" {
		b.WriteString("\n\n")
		if !ws.ReadOnly {
			b.WriteString(AgentTools)
			b.WriteString("\n\n")
		}
		b.WriteString(ConversationTools)
	}
	if r := strings.TrimSpace(turn.Repeat); r != "" {
		b.WriteString("\n\n")
		b.WriteString(r)
	}
	b.WriteString("\n\n")
	b.WriteString(DigestInstruction)
	return b.String()
}

// SummaryPrompt opens a summary: the summarizer's prompt and what to summarise. It touches
// nothing and leaves no digest: it is housekeeping on a conversation, not work on a task.
func SummaryPrompt(job proto.Job, _ *workspace.Workspace) string {
	guide := strings.TrimSpace(job.Guide)
	if guide == "" {
		guide = "Summarise the conversation below for somebody who will not read it."
	}
	return guide + "\n\n---\n\n" + strings.TrimSpace(job.Instruction)
}

func who(job proto.Job) string {
	if job.Agent != nil && job.Agent.Handle != "" {
		return "@" + job.Agent.Handle
	}
	return "an agent"
}
