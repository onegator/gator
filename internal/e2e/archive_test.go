package e2e

import (
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// Archiving hides a project. It does not lose one: the record of what happened there is the
// reason phase transitions and receipts are append-only in the first place.
func TestArchivingHidesAProjectAndKeepsItsHistory(t *testing.T) {
	h := newHarness(t)
	var p gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: uniqueSlug("arch"), Name: "Finished product"}, &p); code != 201 {
		t.Fatalf("project: %d", code)
	}
	id := p.Id.String()
	var task gen.Task
	if code := h.do("POST", "/projects/"+id+"/tasks", gen.NewTask{Kind: "bug", Title: "Something once happened here"}, &task); code != 201 {
		t.Fatalf("task: %d", code)
	}

	if code := h.do("POST", "/projects/"+id+"/archive", gen.ArchiveProject{Archived: true}, nil); code != 200 {
		t.Fatalf("archive: %d", code)
	}

	var live []gen.Project
	h.do("GET", "/projects", nil, &live)
	for _, item := range live {
		if item.Id == p.Id {
			t.Fatal("an archived project should leave the list")
		}
	}
	// The inbox is the screen this most matters for: a finished product must stop asking.
	for _, d := range h.decisions("") {
		if d.Task.ProjectId == p.Id {
			t.Fatal("an archived project still asks for decisions")
		}
	}

	// Everything about it is still there for anyone who asks by id.
	var still gen.Project
	if code := h.do("GET", "/projects/"+id, nil, &still); code != 200 || still.ArchivedAt == nil {
		t.Fatalf("an archived project should still be readable and say so: %d %+v", code, still.ArchivedAt)
	}
	var metrics gen.TaskMetrics
	if code := h.do("GET", "/tasks/"+task.Id.String()+"/metrics", nil, &metrics); code != 200 {
		t.Fatalf("its measurements should survive archiving: %d", code)
	}

	// And it can come back.
	if code := h.do("POST", "/projects/"+id+"/archive", gen.ArchiveProject{Archived: false}, nil); code != 200 {
		t.Fatalf("unarchive: %d", code)
	}
	h.do("GET", "/projects", nil, &live)
	found := false
	for _, item := range live {
		if item.Id == p.Id {
			found = true
		}
	}
	if !found {
		t.Fatal("bringing a project back should put it in the list again")
	}
}

// Deleting is for the project made by mistake five minutes ago, and nothing else.
func TestOnlyAnEmptyProjectIsDeleted(t *testing.T) {
	h := newHarness(t)
	var empty, used gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: uniqueSlug("empty"), Name: "Typo"}, &empty)
	h.do("POST", "/projects", gen.NewProject{Slug: uniqueSlug("used"), Name: "Real work"}, &used)
	var task gen.Task
	h.do("POST", "/projects/"+used.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "Real"}, &task)

	if code := h.do("DELETE", "/projects/"+empty.Id.String(), nil, nil); code != 204 {
		t.Fatalf("an empty project should be deletable: %d", code)
	}
	if code := h.do("GET", "/projects/"+empty.Id.String(), nil, nil); code != 404 {
		t.Fatalf("it should be gone: %d", code)
	}

	var problem gen.Error
	code := h.do("DELETE", "/projects/"+used.Id.String(), nil, &problem)
	if code != 409 {
		t.Fatalf("a project with a task should be refused, got %d", code)
	}
	// The message has to say what to do instead, or a person reaches for the database.
	if !strings.Contains(problem.Error, "archive") {
		t.Errorf("the refusal should point at archiving: %q", problem.Error)
	}
	if code := h.do("GET", "/projects/"+used.Id.String(), nil, nil); code != 200 {
		t.Fatalf("the refused project should still be there: %d", code)
	}
}
