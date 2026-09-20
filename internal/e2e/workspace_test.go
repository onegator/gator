package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

// oneProject makes an empty project to hang a process off.
func (h *harness) oneProject(prefix string) gen.Project {
	h.t.Helper()
	var p gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{
		Slug: fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()), Name: "Workspace"}, &p); code != 201 {
		h.t.Fatalf("project: %d", code)
	}
	return p
}

// sourceOf is where a template in force came from, as a plain string.
func sourceOf(t gen.ProjectTemplate) string {
	if t.Source == nil {
		return ""
	}
	return string(*t.Source)
}

// emptyWorkspaceProcessAfterwards puts the workspace process back. It is one row for the whole
// workspace, so a test that leaves one behind changes the process of every project in the
// database — which is exactly what it did the first time this test ran in the full suite.
func (h *harness) emptyWorkspaceProcessAfterwards() {
	h.t.Cleanup(func() {
		if code := h.do("PUT", "/workspace/process", gen.ProjectConfig{Config: map[string]any{}}, nil); code != 200 {
			h.t.Errorf("restoring the workspace process: %d", code)
		}
	})
}

func (h *harness) template(projectID, kind string) gen.ProjectTemplate {
	h.t.Helper()
	var out []gen.ProjectTemplate
	h.do("GET", "/projects/"+projectID+"/templates", nil, &out)
	for _, t := range out {
		if t.Kind == kind {
			return t
		}
	}
	h.t.Fatalf("no %s template", kind)
	return gen.ProjectTemplate{}
}

// A workspace process: one way of working, without pasting it into every project.
func TestAProjectInheritsTheWorkspaceProcessUnlessItHasItsOwn(t *testing.T) {
	h := newHarness(t)
	h.emptyWorkspaceProcessAfterwards()
	inheriting := h.oneProject("wi")
	overriding := h.oneProject("wo")

	// Before anything is set, a chore follows the defaults in the binary.
	if got := h.template(inheriting.Id.String(), "chore"); sourceOf(got) != "default" {
		t.Fatalf("source = %v", got.Source)
	}

	// The project with its own process keeps it whatever the workspace says.
	projectProcess := map[string]any{"templates": map[string]any{"chore": map[string]any{
		"kind":   "chore",
		"phases": []any{map[string]any{"name": "implementation", "owner": "runner", "role": "worker", "gate": "auto"}},
	}}}
	if code := h.do("PUT", "/projects/"+overriding.Id.String()+"/config",
		gen.ProjectConfig{Config: projectProcess}, nil); code != 200 {
		t.Fatalf("project config: %d", code)
	}

	workspaceProcess := map[string]any{"templates": map[string]any{"chore": map[string]any{
		"kind": "chore",
		"phases": []any{
			map[string]any{"name": "implementation", "owner": "runner", "role": "worker", "gate": "both"},
			map[string]any{"name": "sign_off", "owner": "human", "gate": "human"},
		},
	}}}
	if code := h.do("PUT", "/workspace/process", gen.ProjectConfig{Config: workspaceProcess}, nil); code != 200 {
		t.Fatalf("workspace process: %d", code)
	}

	inherited := h.template(inheriting.Id.String(), "chore")
	if sourceOf(inherited) != "workspace" || len(inherited.Phases) != 2 {
		t.Fatalf("a project without its own process should follow the workspace: %+v", inherited)
	}
	if inherited.Phases[1].Name != "sign_off" {
		t.Errorf("phases = %+v", inherited.Phases)
	}

	kept := h.template(overriding.Id.String(), "chore")
	if sourceOf(kept) != "project" || len(kept.Phases) != 1 {
		t.Fatalf("a project with its own process should keep it: %+v", kept)
	}

	// And the inheritance is live: a task created now walks the workspace's phases.
	var task gen.Task
	if code := h.do("POST", "/projects/"+inheriting.Id.String()+"/tasks",
		gen.NewTask{Kind: gen.NewTaskKindChore, Title: "Follow the workspace"}, &task); code != 201 {
		t.Fatalf("task: %d", code)
	}
	h.do("POST", "/tasks/"+task.Id.String()+"/approve", nil, nil)
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/advance", nil, &task); code != 200 {
		t.Fatalf("advance: %d", code)
	}
	if task.Phase != "sign_off" {
		t.Fatalf("phase = %s", task.Phase)
	}

	// Changing the workspace again must not disturb the project that overrode it.
	workspaceProcess["templates"].(map[string]any)["chore"].(map[string]any)["max_rollbacks"] = 3
	if code := h.do("PUT", "/workspace/process", gen.ProjectConfig{Config: workspaceProcess}, nil); code != 200 {
		t.Fatalf("workspace process: %d", code)
	}
	if after := h.template(overriding.Id.String(), "chore"); sourceOf(after) != "project" || len(after.Phases) != 1 {
		t.Fatalf("the overriding project changed: %+v", after)
	}
}

// A workspace process that would not hold is refused, rather than breaking every project that
// does not override that kind of task.
func TestAnImpossibleWorkspaceProcessIsRefused(t *testing.T) {
	h := newHarness(t)
	bad := map[string]any{"templates": map[string]any{"chore": map[string]any{"kind": "chore", "phases": []any{}}}}
	if code := h.do("PUT", "/workspace/process", gen.ProjectConfig{Config: bad}, nil); code != 400 {
		t.Fatalf("a template with no phases should be refused, got %d", code)
	}
}
