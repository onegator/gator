package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Profile is what the runner knows of one ACP agent beyond the protocol: how to tell it is logged
// in, which of its variables carry the login, and which of its session modes keep it read-only.
// Adapters implement ACP a little differently, so each supported agent has one, recorded against
// its adapter (testdata/<agent>); an agent without a profile runs as experimental.
type Profile struct {
	// Name is the agent's name in acp:<name>.
	Name string
	// Env are the variables the agent needs passed to its process: its login (a cloud runner has
	// no keychain and logs in with one of these) and its configuration. A trailing * matches a prefix.
	Env []string
	// CredentialEnv are the variables of Env that are a login by themselves; AuthState looks at these
	// and at the credential files, never at configuration such as a base URL.
	CredentialEnv []string
	// CredentialFiles are files under the home directory whose presence means a stored login.
	CredentialFiles []string
	// ReadOnlyMode is the session mode that keeps the agent from writing on its own, set for a
	// read-only job when the agent offers it. The permission answers hold either way.
	ReadOnlyMode string
	// Experimental is an agent the runner has no recording of.
	Experimental bool
}

// authState is the login of the agent, as backend.Backend.AuthState reports it: one of its
// variables set or one of its credential files there is "ok", none of them "missing", and an
// agent with nothing to look for "unknown".
func (p Profile) authState(getenv func(string) string, home string) string {
	if len(p.CredentialEnv) == 0 && len(p.CredentialFiles) == 0 {
		return "unknown"
	}
	for _, k := range p.CredentialEnv {
		if getenv(k) != "" {
			return "ok"
		}
	}
	if home != "" {
		for _, f := range p.CredentialFiles {
			if _, err := os.Stat(filepath.Join(home, f)); err == nil {
				return "ok"
			}
		}
	}
	return "missing"
}

var (
	profilesMu sync.RWMutex
	profiles   = map[string]Profile{}
)

// Register adds an agent's profile; a later one of the same name replaces it.
func Register(p Profile) {
	profilesMu.Lock()
	defer profilesMu.Unlock()
	profiles[p.Name] = p
}

// ProfileFor is the agent's profile, or an experimental one when the runner knows nothing of it.
func ProfileFor(name string) Profile {
	profilesMu.RLock()
	defer profilesMu.RUnlock()
	if p, ok := profiles[name]; ok {
		return p
	}
	return Profile{Name: name, Experimental: true}
}

// Known are the agents with a profile, sorted.
func Known() []string {
	profilesMu.RLock()
	defer profilesMu.RUnlock()
	out := make([]string, 0, len(profiles))
	for n := range profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// The agents recorded in testdata. Their logins are what each CLI documents for headless use;
// the credential files are where each keeps a login made on the machine.
func init() {
	// Codex through Zed's codex-acp adapter (testdata/codex: `codex` 0.171.0 --help and
	// login --help; the adapter itself was not installed when this was written).
	Register(Profile{Name: "codex", Env: []string{"OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL", "CODEX_HOME"},
		CredentialEnv: []string{"OPENAI_API_KEY", "CODEX_API_KEY"}, CredentialFiles: []string{".codex/auth.json"}, ReadOnlyMode: "read-only"})
	// Gemini CLI, which speaks ACP itself (`gemini --experimental-acp`).
	Register(Profile{Name: "gemini", Env: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI",
		"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION", "GOOGLE_APPLICATION_CREDENTIALS"},
		CredentialEnv:   []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"},
		CredentialFiles: []string{".gemini/oauth_creds.json"}})
}

// Agent is one ACP agent the runner is configured with: acp:<Name> runs Command.
type Agent struct {
	Name    string
	Command []string
}

// BackendName is the backend an agent is, in policies and runner capabilities.
func (a Agent) BackendName() string { return BackendPrefix + a.Name }

// BackendPrefix names ACP backends: acp:codex.
const BackendPrefix = "acp:"

var agentName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ParseAgents reads GATOR_ACP_AGENTS: name:command pairs separated by commas, where the command
// is a path and its arguments separated by spaces (codex:/usr/local/bin/codex-acp,
// gemini:/usr/local/bin/gemini --experimental-acp). A name appears once.
func ParseAgents(s string) ([]Agent, error) {
	var out []Agent
	seen := map[string]bool{}
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, cmd, ok := strings.Cut(part, ":")
		name = strings.TrimSpace(name)
		if !ok || !agentName.MatchString(name) {
			return nil, fmt.Errorf("GATOR_ACP_AGENTS: %q must be name:command, with a lowercase name", part)
		}
		words := strings.Fields(cmd)
		if len(words) == 0 {
			return nil, fmt.Errorf("GATOR_ACP_AGENTS: %s has no command", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("GATOR_ACP_AGENTS: %s appears twice", name)
		}
		seen[name] = true
		out = append(out, Agent{Name: name, Command: words})
	}
	return out, nil
}
