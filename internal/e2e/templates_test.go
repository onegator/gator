package e2e

import (
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// The board draws its columns from the project's process, including the phases a plugin
// would switch on.
func TestProjectTemplates(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	var templates []gen.ProjectTemplate
	if code := h.do("GET", "/projects/"+task.ProjectId.String()+"/templates", nil, &templates); code != 200 {
		t.Fatalf("templates: %d", code)
	}
	byKind := map[string]gen.ProjectTemplate{}
	for _, tpl := range templates {
		byKind[tpl.Kind] = tpl
	}
	for _, kind := range []string{"feature", "bug", "incident", "chore"} {
		if _, ok := byKind[kind]; !ok {
			t.Fatalf("no template for %s: %v", kind, byKind)
		}
	}
	feature := byKind["feature"]
	var names []string
	release := gen.TemplatePhase{}
	for _, p := range feature.Phases {
		names = append(names, p.Name)
		if p.Name == "release" {
			release = p
		}
	}
	if len(names) < 6 || names[0] != "idea" {
		t.Fatalf("feature phases: %v", names)
	}
	if release.Name == "" || release.Active || release.Requires == nil || *release.Requires != "deploy" {
		t.Fatalf("release waits for a deploy plugin: %+v", release)
	}
	for _, p := range feature.Phases {
		if p.Name == "implementation" && (p.Owner != "runner" || p.Role == nil || *p.Role != "worker") {
			t.Fatalf("implementation is a runner phase: %+v", p)
		}
	}
}
