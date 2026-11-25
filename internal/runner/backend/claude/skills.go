//go:build unix

package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/onegator/gator/internal/runner/backend"
)

// Claude Code reads skills from a plugin: a directory with .claude-plugin/plugin.json and
// skills/<name>/SKILL.md, loaded for one session with --plugin-dir (checked against `claude
// --help` and `claude plugin validate` of 2.1.332, recorded in
// testdata/claude-2.1.332-plugin-validate.json: the directory provides gator-skills:<name>). The
// runner writes it in its own directory, outside the worktree, so the job's git status stays the
// agent's own, and removes it when the run ends.

// skillsPlugin is the plugin's name; the session sees its skills as gator-skills:<name>.
const skillsPlugin = "gator-skills"

// LoadsSkills implements backend.SkillLoader.
func (Backend) LoadsSkills() bool { return true }

var skillName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// skillsDir writes the job's skills as a plugin under dir (the runner's 0700 directory) and
// answers where it is. A file whose path would leave its skill's folder refuses the whole run:
// the server never sends one, and the runner does not take its word for it.
func skillsDir(dir string, skills []backend.Skill) (string, func(), error) {
	nothing := func() {}
	if len(skills) == 0 {
		return "", nothing, nil
	}
	if dir == "" {
		dir = DefaultMCPDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nothing, err
	}
	root, err := os.MkdirTemp(dir, "skills-")
	if err != nil {
		return "", nothing, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	manifest, _ := json.Marshal(map[string]any{"name": skillsPlugin, "version": "1.0.0",
		"description": "The skills Gator handed this job", "author": map[string]string{"name": "Gator"}})
	if err := writeFile(root, ".claude-plugin/plugin.json", manifest, 0o600); err != nil {
		cleanup()
		return "", nothing, err
	}
	for _, sk := range skills {
		if !skillName.MatchString(sk.Name) {
			cleanup()
			return "", nothing, fmt.Errorf("skill %q is not a name", sk.Name)
		}
		for _, f := range sk.Files {
			if !inside(f.Path) {
				cleanup()
				return "", nothing, fmt.Errorf("skill %s: %q leaves its folder", sk.Name, f.Path)
			}
			mode := os.FileMode(0o600)
			if strings.HasPrefix(f.Path, "scripts/") {
				mode = 0o700 // the agent's to run, in this job; nothing ran it before
			}
			if err := writeFile(root, path.Join("skills", sk.Name, f.Path), f.Data, mode); err != nil {
				cleanup()
				return "", nothing, err
			}
		}
	}
	return root, cleanup, nil
}

// inside says whether p stays inside the folder it is relative to.
func inside(p string) bool {
	if p == "" || path.IsAbs(p) || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != p {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return false
		}
	}
	return true
}

func writeFile(root, rel string, data []byte, mode os.FileMode) error {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, data, mode)
}
