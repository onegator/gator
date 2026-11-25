package backend

import (
	"encoding/json"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// Rule is one rule of an approvals policy, in the words Claude Code's permission rules use:
//
//	Bash               the tool, whatever its input
//	Bash(curl:*)       a command whose words start with these ("curl", "git push")
//	Bash(rm -rf /*)    a command that starts with this text (a trailing * is a prefix)
//	WebFetch(domain:example.com)
//	mcp:sentry/*       every tool of an MCP server; mcp:sentry/get_issue one of them
//
// A Bash rule is held to every command a line runs: each part of a pipeline or a chain, a command
// run through bash -c, env, sudo, xargs and the like, and, for a rule of one word, that word
// anywhere in the line. It asks more often than strictly needed rather than miss a command; it is
// a person's look at what the agent does, not a sandbox.
type Rule struct {
	Raw     string
	tool    string // the tool's name; "" with mcpServer set
	pattern string
	words   []string // Bash(…:*): the command's first words
	prefix  bool     // the pattern ends in *
	server  string   // mcp:<server>/…
	mcpTool string   // "*" for any
}

// ParseRule reads one rule; ok is false for one it cannot read.
func ParseRule(s string) (Rule, bool) {
	s = strings.TrimSpace(s)
	r := Rule{Raw: s}
	if rest, ok := strings.CutPrefix(s, "mcp:"); ok {
		server, tool, ok := strings.Cut(rest, "/")
		if !ok || server == "" || tool == "" {
			return Rule{}, false
		}
		r.server, r.mcpTool = server, tool
		return r, true
	}
	name, pattern, hasPattern := strings.Cut(s, "(")
	if name == "" {
		return Rule{}, false
	}
	r.tool = name
	if !hasPattern {
		return r, true
	}
	if !strings.HasSuffix(pattern, ")") {
		return Rule{}, false
	}
	pattern = strings.TrimSuffix(pattern, ")")
	if pattern == "" {
		return Rule{}, false
	}
	if words, ok := strings.CutSuffix(pattern, ":*"); ok {
		r.words = strings.Fields(words)
		if len(r.words) == 0 {
			return Rule{}, false
		}
		return r, true
	}
	r.pattern, r.prefix = strings.CutSuffix(pattern, "*")
	return r, true
}

// ParseRules reads rules, leaving out those it cannot read.
func ParseRules(raw []string) []Rule {
	var out []Rule
	for _, s := range raw {
		if r, ok := ParseRule(s); ok {
			out = append(out, r)
		}
	}
	return out
}

// Matching finds the first rule a tool call matches.
func Matching(rules []Rule, tool string, input []byte) (Rule, bool) {
	for _, r := range rules {
		if r.Matches(tool, input) {
			return r, true
		}
	}
	return Rule{}, false
}

// Matches says whether a call of tool with this input (JSON) falls under the rule.
func (r Rule) Matches(tool string, input []byte) bool {
	if r.server != "" {
		want := "mcp__" + r.server + "__"
		if r.mcpTool == "*" {
			return strings.HasPrefix(tool, want)
		}
		return tool == want+r.mcpTool
	}
	if tool != r.tool {
		return false
	}
	if len(r.words) == 0 && r.pattern == "" && !r.prefix {
		return true
	}
	if tool == "Bash" {
		return r.matchesCommand(inputField(input, "command"))
	}
	if d, ok := strings.CutPrefix(r.pattern, "domain:"); ok {
		u, err := url.Parse(inputField(input, "url"))
		return err == nil && (u.Hostname() == d || strings.HasSuffix(u.Hostname(), "."+d))
	}
	for _, f := range []string{"file_path", "path", "url", "pattern", "query"} {
		if v := inputField(input, f); v != "" {
			return r.matchesText(v)
		}
	}
	return false
}

func (r Rule) matchesText(s string) bool {
	if r.prefix {
		return strings.HasPrefix(s, r.pattern)
	}
	return s == r.pattern
}

// matchesCommand holds a rule to every command a shell line runs.
func (r Rule) matchesCommand(line string) bool {
	if len(r.words) == 0 {
		// A text pattern: the whole line, and each command in it.
		if r.matchesText(strings.TrimSpace(line)) {
			return true
		}
		for _, c := range Commands(line) {
			if r.matchesText(strings.Join(c, " ")) {
				return true
			}
		}
		return false
	}
	for _, c := range Commands(line) {
		if hasWords(c, r.words) {
			return true
		}
	}
	// One word (curl) anywhere in the line: $(which curl), "c"url aside, is still curl.
	if len(r.words) == 1 {
		for _, w := range strings.FieldsFunc(line, func(c rune) bool { return strings.ContainsRune(shellSeparators+" \t\"'=", c) }) {
			if filepath.Base(w) == r.words[0] {
				return true
			}
		}
	}
	return false
}

// hasWords says whether a command starts with words (its program by base name).
func hasWords(cmd, words []string) bool {
	if len(cmd) < len(words) {
		return false
	}
	for i, w := range words {
		got := cmd[i]
		if i == 0 {
			got = path.Base(got)
		}
		if got != w {
			return false
		}
	}
	return true
}

// shellSeparators split a line into commands.
const shellSeparators = ";&|\n`()$<>{}"

// wrappers run the command that follows them.
var wrappers = map[string]bool{"sudo": true, "env": true, "command": true, "exec": true, "nohup": true, "time": true,
	"nice": true, "xargs": true, "timeout": true, "doas": true, "stdbuf": true, "builtin": true}

// Commands are the commands a shell line runs, each as its words, with what wraps them taken away
// (env and VAR=value, sudo, xargs, timeout N, bash -c "…" read as a line of its own).
func Commands(line string) [][]string {
	var out [][]string
	for _, part := range strings.FieldsFunc(line, func(c rune) bool { return strings.ContainsRune(shellSeparators, c) }) {
		words := shellWords(part)
		for len(words) > 0 {
			w := path.Base(words[0])
			switch {
			case strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "="):
				words = words[1:] // VAR=value
				continue
			case wrappers[w]:
				words = words[1:]
				for len(words) > 0 && (strings.HasPrefix(words[0], "-") || isNumber(words[0])) {
					words = words[1:] // their flags and durations
				}
				continue
			case (w == "bash" || w == "sh" || w == "zsh" || w == "dash") && len(words) > 2 && strings.HasPrefix(words[1], "-") && strings.Contains(words[1], "c"):
				out = append(out, Commands(strings.Join(words[2:], " "))...)
				words = nil
				continue
			}
			break
		}
		if len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
}

// shellWords splits a command into words, quotes removed (no expansion).
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, c := range s {
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(c)
		case c == '"' || c == '\'':
			quote = c
		case c == ' ' || c == '\t':
			flush()
		case c == '\\':
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range strings.TrimRight(s, "smhd") {
		if c < '0' || c > '9' {
			if c != '.' {
				return false
			}
		}
	}
	return true
}

// inputField is a string field of a tool's input, "" when there is none.
func inputField(input []byte, name string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	var s string
	_ = json.Unmarshal(m[name], &s)
	return s
}
