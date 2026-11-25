//go:build unix

package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/onegator/gator/internal/runner/backend"
)

var (
	_ = backend.ConfigIsolator(Backend{})
	_ = backend.PayerLoginGuard(Backend{})
)

// ConfigEnv implements backend.ConfigIsolator: Claude Code keeps its settings, sessions and
// transcripts (projects/*.jsonl, with every tool's output) under CLAUDE_CONFIG_DIR.
func (Backend) ConfigEnv(dir string) []string { return []string{"CLAUDE_CONFIG_DIR=" + dir} }

// managedSettings are where an administrator's settings for Claude Code live; they apply whatever
// CLAUDE_CONFIG_DIR says.
func managedSettings() []string {
	if runtime.GOOS == "darwin" {
		return []string{"/Library/Application Support/ClaudeCode/managed-settings.json"}
	}
	return []string{"/etc/claude-code/managed-settings.json"}
}

// PayerLoginBlocked implements backend.PayerLoginGuard. Claude Code prefers an apiKeyHelper, and a
// key or another provider set in a settings file's env, to CLAUDE_CODE_OAUTH_TOKEN: a job on its
// payer's subscription would run, and bill, on that instead. Of the runner's settings only the
// managed ones reach a job on its payer's login, which has a CLAUDE_CONFIG_DIR of its own.
func (b Backend) PayerLoginBlocked() string {
	files := b.ManagedSettings
	if files == nil {
		files = managedSettings()
	}
	for _, f := range files {
		if why := outranksPayer(f); why != "" {
			return why
		}
	}
	return ""
}

// repoBlocked is the same question about the settings a job's checkout carries: a repository's
// .claude/settings.json is anybody's who can push to it.
func repoBlocked(dir string) string {
	for _, f := range []string{".claude/settings.json", ".claude/settings.local.json"} {
		if why := outranksPayer(filepath.Join(dir, f)); why != "" {
			return why
		}
	}
	return ""
}

// outranking are the variables that, set in a settings file's env, decide the login instead of
// the payer's, or send it elsewhere.
var outranking = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"}

// outranksPayer says what in the settings file path would put another login above the payer's.
func outranksPayer(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var s struct {
		APIKeyHelper string            `json:"apiKeyHelper"`
		Env          map[string]string `json:"env"`
	}
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	if s.APIKeyHelper != "" {
		return fmt.Sprintf("%s sets apiKeyHelper, which Claude Code prefers to the payer's login", path)
	}
	for k := range s.Env {
		if slices.Contains(outranking, k) {
			return fmt.Sprintf("%s sets %s, which Claude Code prefers to the payer's login", path, k)
		}
	}
	return ""
}
