package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

func uniqueSlug(prefix string) string { return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()) }

func (h *harness) putComponent(projectID string, in gen.ComponentInput) gen.Component {
	h.t.Helper()
	var out gen.Component
	if code := h.do("PUT", "/projects/"+projectID+"/components", in, &out); code != 200 {
		h.t.Fatalf("put component %s: %d", in.Key, code)
	}
	return out
}

// The catalogue is a prompt, not an encyclopaedia: a job gets the part it is about, what that
// part stands on, and what a change there would reach. Everything else is budget spent on noise.
func TestAJobIsToldAboutItsComponentAndItsNeighbours(t *testing.T) {
	h := newHarness(t)
	var p gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: uniqueSlug("cat"), Name: "Catalogue"}, &p); code != 201 {
		t.Fatalf("project: %d", code)
	}
	id := p.Id.String()

	h.putComponent(id, gen.ComponentInput{Key: "auth", Name: "Auth service", Kind: gen.ComponentInputKindApi,
		Repo: ptr("acme/auth"), Notes: ptr("Issues and checks the session tokens.")})
	h.putComponent(id, gen.ComponentInput{Key: "tokens", Name: "Token library", Kind: gen.ComponentInputKindLib,
		Repo: ptr("acme/tokens")})
	h.putComponent(id, gen.ComponentInput{Key: "web", Name: "Web app", Kind: gen.ComponentInputKindApp,
		DependsOn: &[]string{"auth"}})
	// A part of the product this task has nothing to do with.
	h.putComponent(id, gen.ComponentInput{Key: "billing", Name: "Billing", Kind: gen.ComponentInputKindApi,
		Repo: ptr("acme/billing"), Notes: ptr("Invoices and dunning.")})
	h.putComponent(id, gen.ComponentInput{Key: "auth", Name: "Auth service", Kind: gen.ComponentInputKindApi,
		Repo: ptr("acme/auth"), Notes: ptr("Issues and checks the session tokens."),
		DependsOn: &[]string{"tokens"}})

	var task gen.Task
	if code := h.do("POST", "/projects/"+id+"/tasks", gen.NewTask{Kind: "bug", Title: "Sessions expire early"}, &task); code != 201 {
		t.Fatalf("task: %d", code)
	}
	key := "auth"
	if code := h.do("PUT", "/tasks/"+task.Id.String()+"/component", gen.TaskComponent{Key: &key}, nil); code != 200 {
		t.Fatalf("set component: %d", code)
	}

	body := catalogueDoc(t, h, task.Id.String())
	for _, want := range []string{"Auth service", "acme/auth", "Issues and checks", "Token library", "Web app"} {
		if !strings.Contains(body, want) {
			t.Errorf("the slice should mention %q:\n%s", want, body)
		}
	}
	// The neighbours arrive labelled, so an agent knows which way the arrow points.
	if !strings.Contains(body, "Depends on") || !strings.Contains(body, "Used by") {
		t.Errorf("dependencies should say which direction they run:\n%s", body)
	}
	if strings.Contains(body, "Billing") || strings.Contains(body, "dunning") {
		t.Errorf("an unrelated component leaked into the prompt:\n%s", body)
	}
}

// A component belongs to its project. Nothing about another project's catalogue may appear,
// and a dependency cannot be drawn across the border in the first place.
func TestTheCatalogueStaysInsideItsProject(t *testing.T) {
	h := newHarness(t)
	var a, b gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: uniqueSlug("cata"), Name: "A"}, &a)
	h.do("POST", "/projects", gen.NewProject{Slug: uniqueSlug("catb"), Name: "B"}, &b)
	h.putComponent(a.Id.String(), gen.ComponentInput{Key: "shared-name", Name: "A's service", Kind: gen.ComponentInputKindApi,
		Notes: ptr("Belongs to project A alone.")})
	h.putComponent(b.Id.String(), gen.ComponentInput{Key: "other", Name: "B's service", Kind: gen.ComponentInputKindApi})

	// B cannot depend on A's component, even by its exact key.
	var out gen.Component
	code := h.do("PUT", "/projects/"+b.Id.String()+"/components",
		gen.ComponentInput{Key: "other", Name: "B's service", Kind: gen.ComponentInputKindApi,
			DependsOn: &[]string{"shared-name"}}, &out)
	if code != 400 {
		t.Fatalf("a dependency on another project's component should be refused, got %d", code)
	}

	// Nor can a task in B point at A's component.
	var task gen.Task
	h.do("POST", "/projects/"+b.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "B's bug"}, &task)
	key := "shared-name"
	if code := h.do("PUT", "/tasks/"+task.Id.String()+"/component", gen.TaskComponent{Key: &key}, nil); code != 400 {
		t.Fatalf("a task should not reach another project's component, got %d", code)
	}
}

// Without a component the prompt is what it always was: the catalogue costs nothing until a
// project uses it.
func TestATaskWithNoComponentGetsNoCatalogue(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	for _, d := range h.jobContext(task.Id.String(), "worker") {
		if strings.HasPrefix(d, "catalogue|") {
			t.Fatalf("a task with no component should carry no catalogue: %s", d)
		}
	}
}

// catalogueDoc returns the catalogue slice a worker on this task would read.
func catalogueDoc(t *testing.T, h *harness, taskID string) string {
	t.Helper()
	for _, d := range h.jobContext(taskID, "worker") {
		if strings.HasPrefix(d, "catalogue|") {
			return d
		}
	}
	t.Fatal("no catalogue document in the job context")
	return ""
}
