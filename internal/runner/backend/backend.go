// Package backend is the seam between the runner and agent CLIs. Each backend runs one
// agent process in a directory and reports what happened; the executor owns the job.
package backend

import (
	"context"

	"github.com/onegator/gator/internal/proto"
)

// Spec describes one agent run.
type Spec struct {
	Dir          string
	Prompt       string
	SessionID    string  // resume this session when set
	Model        string  // overrides the backend default
	MaxToolCalls int     // 0 = unlimited
	MaxCostUSD   float64 // dollars this run may spend, for backends that can cap it; 0 = no cap
	// Env is added to the agent's environment, as KEY=VALUE. It carries the job's own identity
	// for gator-cli, and the directory that holds it.
	Env []string
	// DenyRead are files the agent must not open: the runner's token file. Backends pass them
	// to agentenv.Command, which enforces it where the platform can.
	DenyRead []string
	// Access is what the agent may do with its tools. The zero value is everything, as a phase
	// job always could.
	Access Access
	// MCP are the MCP servers the session reaches besides the runner's own configuration: Gator's
	// (the Brain and the agent surface), with the job's own identity.
	MCP []MCPServer
	// Skills are the skills the job's agent carries, for a backend that loads skills by itself
	// (SkillLoader). Any other backend is told of them in its prompt instead.
	Skills []Skill
	// Live is what a backend that keeps its session open (ActiveSteering) is given while the run
	// goes on; nil for a backend that runs one prompt per process.
	Live *Live
}

// Live is the running side of a live session: what a person adds while a turn runs, who answers
// before a tool, and the end of every turn.
type Live struct {
	// Steer carries what a person says while the session runs: a message into the turn, without
	// a restart. The backend reads it while it can still deliver; one it no longer reads is the
	// caller's to send another way (a resumed run).
	Steer <-chan string
	// Approve answers the agent's requests to use a tool; nil leaves the backend's own rules,
	// which never ask anybody.
	Approve Approver
	// TurnEnded is told each time a turn of the session ends (the agent answered), for the caller
	// to checkpoint the worktree; nil for nobody.
	TurnEnded func()
}

// Kinds of a runtime request.
const (
	RequestToolApproval = "tool_approval"
	RequestQuestion     = "question"
)

// ToolRequest is an agent asking, mid-turn, to use a tool — or asking a person a question.
type ToolRequest struct {
	Kind      string // RequestToolApproval | RequestQuestion
	Tool      string
	Input     []byte // the tool's input as JSON
	ToolUseID string
}

// Decision answers a ToolRequest. A denial carries why, which the agent reads; Stop also ends the
// turn. Answer is a person's answer to a question.
type Decision struct {
	Allow   bool
	Message string
	Stop    bool
	Answer  string
}

// Approver decides an agent's requests. It may wait for a person; it must answer before ctx ends.
type Approver interface {
	Decide(ctx context.Context, r ToolRequest) Decision
}

// Skill is one skill of the job's agent: an Agent Skills folder (SKILL.md, scripts/,
// references/, assets/), whole.
type Skill struct {
	Name        string
	Description string
	Files       []SkillFile
}

// SkillFile is one file of a skill, at its path inside the skill's folder.
type SkillFile struct {
	Path string
	Data []byte
}

// SkillLoader is a backend that reads skills by itself: Claude Code, from a plugin directory the
// runner writes outside the worktree. A backend that is not gets the skills' names and
// descriptions in its prompt, and reads one through Gator's skill_read tool.
type SkillLoader interface {
	LoadsSkills() bool
}

// LoadsSkills says whether b reads skills by itself.
func LoadsSkills(b Backend) bool {
	l, ok := b.(SkillLoader)
	return ok && l.LoadsSkills()
}

// MCPServer is one remote MCP server, over Streamable HTTP.
type MCPServer struct {
	Name    string
	URL     string
	Headers map[string]string
}

// Access is what an agent may do with its tools, in words every backend can translate into its
// own flags. It is what the tools allow, not a sandbox: the workspace is made read-only too.
type Access struct {
	// ReadOnly gives the agent tools that read, search and fetch, and none that write.
	ReadOnly bool
	// NoTools gives it none at all: a model reading what it is given and answering.
	NoTools bool
	// Commands are the only commands a read-only agent may run, by name: gator-cli, to talk back.
	Commands []string
}

// Emit streams one event.
type Emit func(typ string, payload any)

// Outcome is what one run produced.
type Outcome struct {
	Status      string // proto.StatusDone | proto.StatusFailed; empty when Interrupted
	Reason      string
	Interrupted bool  // the caller's context ended the run (stop, timeout, steer)
	Cause       error // context cause when Interrupted
	SessionID   string
	Model       string
	ToolCalls   int
	Summary     string
	Usage       proto.Usage
	// Unread are messages a live session took from Live.Steer and could not deliver, its input
	// closed: the caller's to send in the next run.
	Unread []string
}

// Backend runs an agent CLI.
type Backend interface {
	Name() string
	Run(ctx context.Context, spec Spec, emit Emit) Outcome
	// AuthState reports the login of this backend: "ok", "missing" (the command is there, the
	// credentials are not), "no_cli" (the command itself is not on this runner's PATH) or
	// "unknown". A missing command and a missing login are different problems with different
	// fixes, and reporting the first as the second sends a person to log in again on a machine
	// they were already logged in on.
	AuthState() string
	// Capabilities says what the backend can do while a job runs (steer without a restart, stop
	// without a kill, ask before a tool, take attachments), for the runner to report and the
	// server to show only the actions a job's backend can carry out.
	Capabilities() proto.BackendCapabilities
}

// Limited is a backend that can hold an agent to less than full access. A backend says which
// limits it can keep; one that cannot keep a limit never runs a job that needs it.
type Limited interface {
	Supports(a Access) bool
}

// Supports says whether b can run an agent with access a: full access always, anything less only
// where b says it can keep it.
func Supports(b Backend, a Access) bool {
	if !a.ReadOnly && !a.NoTools {
		return true
	}
	l, ok := b.(Limited)
	return ok && l.Supports(a)
}
