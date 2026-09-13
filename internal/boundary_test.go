package internal

import (
	"os/exec"
	"strings"
	"testing"
)

// TestImportBoundary enforces the one rule that keeps gator-server and gator-runner
// separable: neither internal/server nor internal/runner may import the other.
// The only package both may share is internal/proto.
func TestImportBoundary(t *testing.T) {
	cases := map[string]string{
		"./server/...": "github.com/onegator/gator/internal/runner",
		"./runner/...": "github.com/onegator/gator/internal/server",
	}
	for pattern, forbidden := range cases {
		out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", pattern).CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", pattern, err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, forbidden) {
				t.Errorf("%s imports %s, which crosses the server/runner boundary", pattern, line)
			}
		}
	}
}
