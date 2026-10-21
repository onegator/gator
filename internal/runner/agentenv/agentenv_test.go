package agentenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFilterKeepsWhatAnAgentNeedsAndNothingOfTheRunner(t *testing.T) {
	env := []string{
		"PATH=/usr/bin", "HOME=/home/a", "LANG=C.UTF-8", "LC_ALL=C", "SSH_AUTH_SOCK=/tmp/s",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth", "ANTHROPIC_API_KEY=key",
		"GATOR_RUNNER_TOKEN=gtr_runner_secret", "GATOR_RUNNER_TOKEN_FILE=/x/token", "GATOR_URL=https://stale",
		"CLAUDECODE=1", "AWS_SECRET_ACCESS_KEY=aws", "DATABASE_URL=postgres://", "MY_TOOL=1",
	}
	got := Filter(env, []string{"MY_*", "GATOR_RUNNER_TOKEN"})
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/a", "LANG=C.UTF-8", "LC_ALL=C", "SSH_AUTH_SOCK=/tmp/s",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth", "ANTHROPIC_API_KEY=key", "MY_TOOL=1"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s was dropped", want)
		}
	}
	for _, kv := range got {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GATOR_") || k == "CLAUDECODE" || k == "AWS_SECRET_ACCESS_KEY" || k == "DATABASE_URL" {
			t.Errorf("%s reached the agent", k)
		}
	}
}

// The token file is the runner's; an agent running as the same user must not open it.
func TestSandboxDeniesTheTokenFile(t *testing.T) {
	if !Sandboxed() {
		t.Skip("no sandbox on this platform; docs/deploy.md says how a Linux runner keeps its token away")
	}
	dir := t.TempDir()
	token := filepath.Join(dir, "runner-token")
	other := filepath.Join(dir, "notes")
	for _, p := range []string{token, other} {
		if err := os.WriteFile(p, []byte("gtr_runner_secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin, args := Command("/bin/cat", []string{token}, []string{token})
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err == nil || strings.Contains(string(out), "gtr_runner_secret") {
		t.Fatalf("the agent read the token: %s (%v)", out, err)
	}
	bin, args = Command("/bin/cat", []string{other}, []string{token})
	if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("the agent lost a file it should keep: %s (%v)", out, err)
	}
}

func TestProfileQuotes(t *testing.T) {
	p := Profile([]string{`/tmp/a "b"\c`})
	if !strings.Contains(p, `(literal "/tmp/a \"b\"\\c")`) {
		t.Fatalf("profile: %s", p)
	}
}
