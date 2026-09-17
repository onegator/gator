package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/knowledge"
)

// writeRegistry builds a one-pack registry in a git repository and returns its file:// url.
func writeRegistry(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "go-service")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := knowledge.Manifest{Name: "go-service", Version: "1.0.0", Scope: "language",
		Roles: []string{"worker", "reviewer"}, Files: []string{"style.md"}}
	style := "# Go here\nSmall packages, no stutter."
	writeFile(t, filepath.Join(packDir, "pack.json"), toJSON(t, manifest))
	writeFile(t, filepath.Join(packDir, "style.md"), style)
	pack := knowledge.Pack{Manifest: manifest, Files: []knowledge.File{{Path: "style.md", Content: style}}}
	index := knowledge.Index{Packs: []knowledge.IndexEntry{
		{Name: "go-service", Version: "1.0.0", Path: "packs/go-service", Checksum: pack.Checksum()}}}
	writeFile(t, filepath.Join(dir, "index.json"), toJSON(t, index))

	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "registry"}} {
		cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return "file://" + dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A pack fetched from a registry reaches the roles it names, a project's own entry overrides
// it, and none of it crosses into another project's prompt except what the workspace shares.
func TestKnowledgePacksEntriesAndOverrides(t *testing.T) {
	h := newHarness(t)
	here, elsewhere := h.task(), h.task()

	var packs []gen.KnowledgePack
	if code := h.do("POST", "/packs", gen.FetchPacks{Url: writeRegistry(t)}, &packs); code != 200 {
		t.Fatalf("fetch: %d", code)
	}
	if len(packs) != 1 || packs[0].Name != "go-service" || packs[0].Scope != "language" {
		t.Fatalf("fetched: %+v", packs)
	}

	if code := h.do("PUT", "/projects/"+here.ProjectId.String()+"/packs", gen.ProjectPack{Name: "go-service"}, nil); code != 204 {
		t.Fatalf("enable: %d", code)
	}
	worker := strings.Join(h.jobContext(here.Id.String(), "worker"), "\n")
	if !strings.Contains(worker, "knowledge|language: go-service (pack 1.0.0)") || !strings.Contains(worker, "no stutter") {
		t.Fatalf("a worker should read the pack:\n%s", worker)
	}
	if planner := strings.Join(h.jobContext(here.Id.String(), "planner"), "\n"); strings.Contains(planner, "go-service") {
		t.Fatalf("the pack names worker and reviewer, not planner:\n%s", planner)
	}
	if other := strings.Join(h.jobContext(elsewhere.Id.String(), "worker"), "\n"); strings.Contains(other, "go-service") {
		t.Fatalf("another project did not switch it on:\n%s", other)
	}

	// The workspace's own entry reaches every project. It is workspace-wide, so this test
	// takes it away again rather than leaving it in every other test's prompt.
	var shared gen.KnowledgeEntry
	if code := h.do("POST", "/knowledge", gen.NewKnowledgeEntry{Scope: "security", Title: "Secrets", Content: "Secrets live in the keyring, never in a prompt."}, &shared); code != 201 {
		t.Fatalf("workspace entry: %d", code)
	}
	t.Cleanup(func() { h.do("DELETE", "/knowledge/"+shared.Id.String(), nil, nil) })
	for _, task := range []gen.Task{here, elsewhere} {
		if got := strings.Join(h.jobContext(task.Id.String(), "worker"), "\n"); !strings.Contains(got, "knowledge|security: Secrets") {
			t.Fatalf("every project reads workspace knowledge:\n%s", got)
		}
	}

	// A project's entry with the pack's scope and name hides the pack, and says so.
	if code := h.do("POST", "/projects/"+here.ProjectId.String()+"/knowledge",
		gen.NewKnowledgeEntry{Scope: "language", Title: "go-service", Content: "Here we also run golangci-lint."}, nil); code != 201 {
		t.Fatalf("project entry: %d", code)
	}
	var preview gen.KnowledgePreview
	if code := h.do("GET", "/projects/"+here.ProjectId.String()+"/knowledge/preview?role=worker&phase=implementation", nil, &preview); code != 200 {
		t.Fatalf("preview: %d", code)
	}
	if preview.Bytes == 0 || len(preview.Documents) < 2 {
		t.Fatalf("preview: %+v", preview)
	}
	if !strings.Contains(strings.Join(preview.Warnings, "\n"), "pack go-service is hidden by the language entry") {
		t.Fatalf("the preview should say what is hidden: %+v", preview.Warnings)
	}
	after := strings.Join(h.jobContext(here.Id.String(), "worker"), "\n")
	if !strings.Contains(after, "golangci-lint") || strings.Contains(after, "no stutter") {
		t.Fatalf("the project's entry replaces the pack:\n%s", after)
	}
}
