// Package jobscope says what a job's own identity may call, by the job's kind. The agent surface
// over REST (/agent/*, gator-cli) and the MCP server ask the same sets, so a tool cannot reach in
// one door what the other refuses.
package jobscope

// Command is one call of the agent surface, named as gator-cli names it.
type Command string

const (
	TaskShow       Command = "task show"
	TaskBlocked    Command = "task blocked"
	Ask            Command = "ask"
	ArtifactPut    Command = "artifact put"
	ProductPropose Command = "product propose"
	CatalogueShow  Command = "catalogue show"
	Knowledge      Command = "knowledge search"
	Say            Command = "say"
	ThreadShow     Command = "thread show"
	TaskCreate     Command = "task create"
	// SkillRead reads a skill the job's agent carries; for backends that do not load skills by
	// themselves (phase 4).
	SkillRead Command = "skill read"
	// MCPProxy reaches the remote MCP servers the job's agent was given, through Gator's proxy,
	// with the tools the server's allowlist and the agent's grant both hold (phase 4).
	MCPProxy Command = "mcp proxy"
	// AttachmentRead fetches a file sent in the conversation a reply answers in (phase 7).
	AttachmentRead Command = "attachment read"
)

// Set is a set of commands.
type Set map[Command]bool

// Of builds a set.
func Of(cs ...Command) Set {
	out := Set{}
	for _, c := range cs {
		out[c] = true
	}
	return out
}

// Kinds of jobs (jobs.kind).
const (
	KindPhase   = "phase"
	KindReply   = "reply"
	KindSummary = "summary"
)

// A phase job works on its phase: it blocks the gate, asks, records documents. A reply answers in
// a conversation and touches no phase — consented or not, blocking a gate or writing its documents
// is not what anyone asked it for — so it talks, looks things up and proposes. A summary is a cheap
// model reading a transcript with no identity at all.
var (
	phase = Of(TaskShow, TaskBlocked, Ask, ArtifactPut, ProductPropose, CatalogueShow, Knowledge, Say, ThreadShow, TaskCreate,
		SkillRead, MCPProxy)
	reply = Of(Say, ThreadShow, TaskCreate, Knowledge, CatalogueShow, ProductPropose, SkillRead, MCPProxy, AttachmentRead)
)

// For is what a job of kind may call; anything unknown may call nothing.
func For(kind string) Set {
	switch kind {
	case KindPhase:
		return phase
	case KindReply:
		return reply
	}
	return Set{}
}

// Active says whether a job in status still has its identity: the token is revoked when a job
// ends, so a finished job calling is a race, not a hole.
func Active(status string) bool {
	switch status {
	case "queued", "leased", "running", "stalled":
		return true
	}
	return false
}
