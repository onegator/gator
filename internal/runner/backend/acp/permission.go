package acp

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"

	"github.com/onegator/gator/internal/runner/backend"
)

// Tool kinds an agent names its tool calls with.
const (
	kindRead    = "read"
	kindEdit    = "edit"
	kindDelete  = "delete"
	kindMove    = "move"
	kindSearch  = "search"
	kindExecute = "execute"
	kindThink   = "think"
	kindFetch   = "fetch"
	kindOther   = "other"
)

// readingKinds are what a read-only agent may do without asking anyone: read, search, fetch and
// think. Writing, moving, deleting and running commands are refused; so is anything the agent
// does not name, unless it is one of the job's own MCP servers.
var readingKinds = []string{kindRead, kindSearch, kindFetch, kindThink}

// Permissions answers an agent's session/request_permission for the job's access: the tier of the
// job, not the agent's own defaults. Full access allows everything (a phase job, as Claude Code's
// bypassPermissions); read-only allows reading, the named commands and the job's MCP servers;
// no tools allows nothing.
type Permissions struct {
	Access backend.Access
	// MCP are the job's own MCP servers, by name: their tools are held by the server to what the
	// job may call, so a read-only session may use them as a Claude Code one does.
	MCP []string
}

// Verdict is a permission answer: allowed or not, and why, for the job's events.
type Verdict struct {
	Allow  bool
	Reason string
}

// Decide answers one request.
func (p Permissions) Decide(call permissionToolCall) Verdict {
	switch {
	case p.Access.NoTools:
		return Verdict{Reason: "this job has no tools"}
	case !p.Access.ReadOnly:
		return Verdict{Allow: true, Reason: "full access"}
	case slices.Contains(readingKinds, call.Kind):
		return Verdict{Allow: true, Reason: "read-only: " + call.Kind}
	case call.Kind == kindExecute:
		if cmd, ok := p.allowedCommand(call.RawInput); ok {
			return Verdict{Allow: true, Reason: "read-only: " + cmd}
		}
		return Verdict{Reason: "read-only: commands other than " + strings.Join(p.Access.Commands, ", ") + " are refused"}
	case (call.Kind == "" || call.Kind == kindOther) && p.ownMCP(call.Title):
		// Only a call the agent does not name as editing, deleting or moving may be one of the
		// job's MCP tools: a title is the agent's free text, and an edit's is a path.
		return Verdict{Allow: true, Reason: "read-only: the job's MCP server"}
	}
	kind := call.Kind
	if kind == "" {
		kind = kindOther
	}
	return Verdict{Reason: "read-only: " + kind + " is refused"}
}

// shellMeta are what would let a permitted command run another: a read-only agent's gator-cli is
// one command, never a pipeline.
const shellMeta = ";|&`$<>\n(){}"

// allowedCommand says whether a command an agent wants to run is one of the job's named
// commands, alone: its first word, by base name, with nothing that chains another command.
func (p Permissions) allowedCommand(raw json.RawMessage) (string, bool) {
	words := commandWords(raw)
	if len(words) == 0 {
		return "", false
	}
	for _, w := range words {
		if strings.ContainsAny(w, shellMeta) {
			return "", false
		}
	}
	// By its name alone, found on PATH (where the executor puts the runner's own gator-cli first):
	// ./gator-cli or tools/gator-cli could be a file the repository brought.
	if strings.ContainsRune(words[0], '/') {
		return "", false
	}
	return words[0], slices.Contains(p.Access.Commands, words[0])
}

// commandWords reads the command of an execute tool call, which agents send as {"command":
// "gator-cli say"} or {"command": ["gator-cli", "say"]} (with or without a shell wrapper).
func commandWords(raw json.RawMessage) []string {
	var in struct {
		Command json.RawMessage `json:"command"`
		Cmd     json.RawMessage `json:"cmd"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	c := in.Command
	if len(c) == 0 {
		c = in.Cmd
	}
	var s string
	if json.Unmarshal(c, &s) == nil {
		return strings.Fields(s)
	}
	var list []string
	if json.Unmarshal(c, &list) != nil {
		return nil
	}
	// ["bash", "-lc", "gator-cli say …"] is the shell's: what it runs is the last word.
	if len(list) == 3 && (filepath.Base(list[0]) == "bash" || filepath.Base(list[0]) == "sh") && strings.HasPrefix(list[1], "-") {
		return strings.Fields(list[2])
	}
	return list
}

// ownMCP says whether a tool call's title names one of the job's MCP servers, in the forms agents
// use: mcp__gator__brain_search, gator__brain_search, gator/brain_search, gator: brain_search.
func (p Permissions) ownMCP(title string) bool {
	t := strings.TrimSpace(title)
	t = strings.TrimPrefix(t, "mcp__")
	for _, name := range p.MCP {
		for _, sep := range []string{"__", "/", ":", "."} {
			if strings.HasPrefix(t, name+sep) {
				return true
			}
		}
	}
	return false
}

// answer picks the option that says the verdict once: allow_once (never allow_always — the next
// call is asked again) or reject_once, falling back to the always variant, and cancels the
// request when the agent offered neither.
func answer(v Verdict, options []permissionOption) permissionResult {
	prefer := []string{"reject_once", "reject_always"}
	if v.Allow {
		prefer = []string{"allow_once", "allow_always"}
	}
	for _, kind := range prefer {
		for _, o := range options {
			if o.Kind == kind {
				return permissionResult{Outcome: permissionOutcome{Outcome: "selected", OptionID: o.OptionID}}
			}
		}
	}
	return permissionResult{Outcome: permissionOutcome{Outcome: "cancelled"}}
}
