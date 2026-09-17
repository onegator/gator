package knowledge

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeRegistry builds a registry on disk and returns its directory.
func writeRegistry(t *testing.T, checksumOf func(Pack) string) string {
	t.Helper()
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "go-service")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Name: "go-service", Version: "1.0.0", Scope: "language", AppliesTo: []string{"go"},
		Roles: []string{"worker", "reviewer"}, Files: []string{"style.md", "testing.md"}}
	write(t, filepath.Join(packDir, "pack.json"), mustJSON(t, manifest))
	write(t, filepath.Join(packDir, "style.md"), "# Style\nSmall packages, no stutter.")
	write(t, filepath.Join(packDir, "testing.md"), "# Testing\nTable tests stay readable.")
	pack := Pack{Manifest: manifest, Files: []File{
		{Path: "style.md", Content: "# Style\nSmall packages, no stutter."},
		{Path: "testing.md", Content: "# Testing\nTable tests stay readable."},
	}}
	index := Index{Packs: []IndexEntry{{Name: "go-service", Version: "1.0.0", Path: "packs/go-service", Checksum: checksumOf(pack)}}}
	write(t, filepath.Join(dir, "index.json"), mustJSON(t, index))
	return dir
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReadRegistry(t *testing.T) {
	dir := writeRegistry(t, Pack.Checksum)
	packs, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 1 || packs[0].Manifest.Name != "go-service" || len(packs[0].Files) != 2 {
		t.Fatalf("packs: %+v", packs)
	}
	if text := packs[0].Text(); !strings.HasPrefix(text, "# Style") || !strings.Contains(text, "# Testing") {
		t.Fatalf("text keeps the manifest's order:\n%s", text)
	}
}

func TestAPackThatDoesNotMatchItsChecksumIsRefused(t *testing.T) {
	dir := writeRegistry(t, func(Pack) string { return "sha256:0000" })
	_, err := Read(dir)
	if err == nil || !strings.Contains(err.Error(), "the files hash to") {
		t.Fatalf("a tampered pack must be refused: %v", err)
	}
}

func TestFetchClonesARegistry(t *testing.T) {
	dir := writeRegistry(t, Pack.Checksum)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "registry")

	packs, err := Fetch(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 1 || packs[0].Manifest.Scope != "language" {
		t.Fatalf("fetched: %+v", packs)
	}
}

func TestAppliesToRolePhaseAndTags(t *testing.T) {
	m := Manifest{Roles: []string{"worker"}, Phases: []string{"implementation"}, AppliesTo: []string{"go"}}
	if !m.Applies("worker", "implementation", []string{"go", "api"}) {
		t.Fatal("this pack belongs here")
	}
	if m.Applies("planner", "implementation", []string{"go"}) {
		t.Fatal("wrong role")
	}
	if m.Applies("worker", "planning", []string{"go"}) {
		t.Fatal("wrong phase")
	}
	if m.Applies("worker", "implementation", []string{"swift"}) {
		t.Fatal("wrong project")
	}
	open := Manifest{}
	if !open.Applies("anyone", "anywhere", nil) {
		t.Fatal("a pack that names nothing applies everywhere")
	}
}

func TestManifestValidation(t *testing.T) {
	for _, m := range []Manifest{
		{Name: "Bad Name", Version: "1", Scope: "language", Files: []string{"a.md"}},
		{Name: "ok", Version: "", Scope: "language", Files: []string{"a.md"}},
		{Name: "ok", Version: "1", Scope: "vibes", Files: []string{"a.md"}},
		{Name: "ok", Version: "1", Scope: "language"},
		{Name: "ok", Version: "1", Scope: "language", Files: []string{"../secrets.md"}},
		{Name: "ok", Version: "1", Scope: "language", Files: []string{"notes.txt"}},
	} {
		if err := m.Validate(); err == nil {
			t.Errorf("should be refused: %+v", m)
		}
	}
	good := Manifest{Name: "go-service", Version: "1.0.0", Scope: "language", Files: []string{"style.md"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
}
