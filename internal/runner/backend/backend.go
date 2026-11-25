// Package backend is the seam between the runner and agent CLIs. Each backend runs one
// agent process in a directory and reports what happened; the executor owns the job.
package backend

import (
	"context"
	"os"
	"regexp"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/agentenv"
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
	// PayerLogin says Env carries the model login of the person who pays for the job: the agent
	// gets that one and none of the runner's own (agentenv.WithoutLogins).
	PayerLogin bool
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
	// LoginRefused is the CLI's own words, when the CLI itself — not its model — said its login was
	// turned down: an error result, an RPC error, what it printed on stderr before it died. Never
	// what the agent wrote, which a task about invalid keys, or a page it read, could fill.
	LoginRefused string
}

// refusals are how the CLIs say a login was turned down: an invalid or revoked key, an expired
// OAuth token, a ChatGPT login whose refresh token was used elsewhere.
var refusals = regexp.MustCompile(`(?i)invalid (x-)?api[ _-]?key|authentication_error|authentication_failed|authentication required|invalid bearer token|` +
	`oauth token (has )?(expired|been revoked)|token (has )?(expired|been revoked)|please run /login|not logged in|` +
	`refresh_token_reused|refresh token (was|has been) (already )?used|could not be refreshed|\b401\b[^\n]{0,40}(unauthori[sz]ed|authentication)`)

// LoginRefusal is text when a CLI's own error says its login was turned down, else "".
func LoginRefusal(text string) string {
	if refusals.MatchString(text) {
		return text
	}
	return ""
}

// AgentEnv is the environment of an agent's process: what it may inherit of the runner's (allow
// names more than agentenv's list), without the runner's own logins when the job brings its
// payer's, and the job's own variables last, so they win.
func AgentEnv(spec Spec, allow []string) []string {
	base := agentenv.Filter(os.Environ(), allow)
	if spec.PayerLogin {
		base = agentenv.WithoutLogins(base)
	}
	return append(base, spec.Env...)
}

// ConfigIsolator is a backend whose CLI keeps its settings, sessions and transcripts in a
// directory a variable names. A job on its payer's login gets a directory of its own, so what one
// person's job wrote — a transcript holding tool output, a settings file — is not another's to read
// or to be steered by.
type ConfigIsolator interface {
	ConfigEnv(dir string) []string
}

// PayerLoginGuard is a backend that can tell when something would put another login above a
// payer's: a managed setting that supplies a key, a repository's settings that do. Blocked says
// why, or "" when nothing does.
type PayerLoginGuard interface {
	// PayerLoginBlocked looks at the runner's own configuration, once, at start.
	PayerLoginBlocked() string
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
