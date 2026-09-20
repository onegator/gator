package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The Mac runner is configured by a launchd plist, which is not a place for a secret.
func TestTokenFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("gtr_runner_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATOR_RUNNER_SERVER_URL", "https://gator.example")
	t.Setenv("GATOR_RUNNER_TOKEN_FILE", path)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "gtr_runner_secret" {
		t.Errorf("token = %q; the trailing newline of a written file is not part of it", c.Token)
	}

	// An explicit token wins: a plist that still carries one keeps working.
	t.Setenv("GATOR_RUNNER_TOKEN", "from-the-environment")
	if c, err := Load(); err != nil || c.Token != "from-the-environment" {
		t.Errorf("token = %q (%v)", c.Token, err)
	}
}

// A token file that is named but missing is a misconfiguration, not an empty token: starting
// anyway would register nothing and look like a network problem.
func TestMissingTokenFileIsAnError(t *testing.T) {
	t.Setenv("GATOR_RUNNER_SERVER_URL", "https://gator.example")
	t.Setenv("GATOR_RUNNER_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))
	if _, err := Load(); err == nil {
		t.Fatal("a missing token file should fail loudly")
	}
}
