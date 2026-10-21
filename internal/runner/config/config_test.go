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

func TestPlacementFollowsLocationUnlessNamed(t *testing.T) {
	t.Setenv("GATOR_RUNNER_SERVER_URL", "https://gator.example")
	t.Setenv("GATOR_RUNNER_TOKEN", "t")
	for loc, want := range map[string]string{"vps": "cloud", "mac": "local", "other": "local"} {
		t.Setenv("GATOR_RUNNER_LOCATION", loc)
		if c, err := Load(); err != nil || c.Placement != want {
			t.Errorf("%s: %q (%v), want %s", loc, c.Placement, err, want)
		}
	}
	t.Setenv("GATOR_RUNNER_PLACEMENT", "local")
	t.Setenv("GATOR_RUNNER_LOCATION", "vps")
	if c, _ := Load(); c.Placement != "local" {
		t.Errorf("an explicit placement lost to the location: %q", c.Placement)
	}
	t.Setenv("GATOR_RUNNER_PLACEMENT", "moon")
	if _, err := Load(); err == nil {
		t.Error("an unknown placement was accepted")
	}
}

// A slug in the project list used to mean "any project" by the time it reached the server.
func TestProjectsMustBeIDs(t *testing.T) {
	t.Setenv("GATOR_RUNNER_SERVER_URL", "https://gator.example")
	t.Setenv("GATOR_RUNNER_TOKEN", "t")
	t.Setenv("GATOR_RUNNER_PROJECTS", "0b5b5e6a-5a7e-4c38-9a53-1f0f1a2b3c4d,my-project")
	if _, err := Load(); err == nil {
		t.Fatal("a project slug was accepted")
	}
	t.Setenv("GATOR_RUNNER_PROJECTS", "0b5b5e6a-5a7e-4c38-9a53-1f0f1a2b3c4d")
	if c, err := Load(); err != nil || len(c.Projects) != 1 {
		t.Fatalf("%v %v", c.Projects, err)
	}
}
