package workspace

import (
	"fmt"
	"os"
	"path/filepath"
)

// signHook adds the trailer, kept next to it, to every commit message; once, however often the
// message is edited.
const signHook = `#!/bin/sh
# Written by gator-runner: every commit of this job names the agent that made it.
trailer=$(cat "$(dirname "$0")/trailer")
exec git interpret-trailers --in-place --if-exists addIfDifferent --trailer "$trailer" "$1"
`

// SignCommits has every commit made in the workspace carry trailer (Co-Authored-By: @worker via
// Gator), whoever runs git commit: the agent, a tool, a script. The hook lives outside the
// checkout, so the agent does not commit it, and is found through the environment it returns
// for the agent's process (git reads GIT_CONFIG_* before any file). A read-only checkout or a
// job without a repository commits nothing and needs none.
func (w *Workspace) SignCommits(trailer string) ([]string, error) {
	if !w.HasRepo() || w.ReadOnly || trailer == "" {
		return nil, nil
	}
	dir := filepath.Join(w.m.Root, "hooks", filepath.Base(w.Dir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "trailer"), []byte(trailer), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "prepare-commit-msg"), []byte(signHook), 0o755); err != nil {
		return nil, err
	}
	w.hooks = dir
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", fmt.Sprintf("GIT_CONFIG_VALUE_0=%s", dir)}, nil
}
