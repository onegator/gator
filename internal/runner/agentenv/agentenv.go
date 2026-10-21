// Package agentenv decides what an agent's process inherits from the runner. The agent runs
// with bypassPermissions as the runner's own user, so whatever reaches its environment, or a
// file it can open, is the agent's to read, print and send anywhere. The runner's token must
// be neither: it leases any job the runner may run, and an agent working on one task's words
// — some of them a stranger's — must not be able to act for every other.
package agentenv

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// allowed is what an agent may inherit: enough to find its tools, run git over ssh, speak
// through a proxy and log its backend in. Entries ending in * are prefixes.
var allowed = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP", "TERM", "COLORTERM",
	"LANG", "LANGUAGE", "LC_*", "TZ", "XDG_*",
	"SSH_AUTH_SOCK", "SSH_AGENT_PID",
	"GIT_SSH", "GIT_SSH_COMMAND", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	// Backend logins. A cloud machine has no keychain; its login is one of these.
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
}

// never are dropped whatever the allow-list says. GATOR_* is the runner's configuration and its
// token; the job's own GATOR_URL and GATOR_JOB_TOKEN are added after filtering, by the
// executor. The CLAUDE* ones make Claude Code believe it runs nested inside another session.
var never = []string{"GATOR_*", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT"}

func matches(patterns []string, key string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			return strings.HasPrefix(key, prefix)
		}
		return p == key
	})
}

// Filter keeps the variables of env (KEY=VALUE) the agent may have: the built-in allow-list
// plus extra, which names more (a trailing * matches a prefix). GATOR_* never passes; what the
// job needs of Gator, the executor adds afterwards.
func Filter(env []string, extra []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if matches(never, k) {
			continue
		}
		if matches(allowed, k) || matches(extra, k) {
			out = append(out, kv)
		}
	}
	return out
}

// Sandboxed says whether Command can keep the agent away from files on this machine.
func Sandboxed() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath(sandboxExec)
	return err == nil
}

const sandboxExec = "/usr/bin/sandbox-exec"

// Command returns the program and arguments that run bin with args such that the files in
// denyRead cannot be opened. On macOS that is sandbox-exec with a profile that allows
// everything but those files: the agent keeps its whole toolbox and loses only the secret.
// Elsewhere it returns bin and args unchanged — a Linux runner keeps its token out of reach
// by other means (see docs/deploy.md), and pretending otherwise here would be worse than
// saying so.
func Command(bin string, args, denyRead []string) (string, []string) {
	if len(denyRead) == 0 || !Sandboxed() {
		return bin, args
	}
	return sandboxExec, append([]string{"-p", Profile(denyRead), bin}, args...)
}

// Profile is the sandbox profile denying the files. Each is denied under its own name and the
// name symlinks resolve to, because the sandbox checks the real path: on a Mac /var is
// /private/var and a rule for one does not cover the other.
func Profile(denyRead []string) string {
	var paths []string
	for _, p := range denyRead {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		paths = append(paths, p)
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
			paths = append(paths, real)
		}
	}
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "(deny file-read* file-write* (literal %s))\n", quote(p))
	}
	return b.String()
}

// quote writes p as a sandbox profile string literal.
func quote(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(p) + `"`
}
