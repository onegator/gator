package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

func ptr[T any](v T) *T { return &v }

func TestAnyoneSignedInRunsTheirOwnProject(t *testing.T) {
	h := newHarness(t)
	admin := h.token
	_, person := h.login("member")
	h.token = person

	slug := fmt.Sprintf("own%d", time.Now().UnixNano())
	var project gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: slug, Name: "Own", RepoUrl: ptr("onegator/gator"), DefaultBranch: ptr("develop")}, &project); code != 201 {
		t.Fatalf("a member creating a project: %d", code)
	}
	if project.Role == nil || *project.Role != gen.ProjectRoleAdmin {
		t.Fatalf("the creator should run the project: %+v", project.Role)
	}
	var listed []gen.Project
	h.do("GET", "/projects", nil, &listed)
	if len(listed) != 1 || listed[0].Id != project.Id || listed[0].Role == nil || *listed[0].Role != gen.ProjectRoleAdmin {
		t.Fatalf("the creator's list: %+v", listed)
	}
	var repos []gen.ProjectRepo
	h.do("GET", "/projects/"+project.Id.String()+"/repos", nil, &repos)
	if len(repos) != 1 || repos[0].Url != "https://github.com/onegator/gator.git" || *repos[0].DefaultBranch != "develop" || !*repos[0].Primary {
		t.Fatalf("the repository given at creation: %+v", repos)
	}

	if code := h.do("POST", "/projects", gen.NewProject{Slug: slug, Name: "Again"}, nil); code != 409 {
		t.Fatalf("a taken short name: %d", code)
	}
	if code := h.do("POST", "/projects", gen.NewProject{Slug: slug + "x", Name: "Bad", RepoUrl: ptr("--upload-pack=x")}, nil); code != 400 {
		t.Fatalf("a repository git would read as an option: %d", code)
	}

	// The project admin archives and brings it back; deleting stays with workspace admins.
	if code := h.do("POST", "/projects/"+project.Id.String()+"/archive", gen.ArchiveProject{Archived: true}, &project); code != 200 || project.ArchivedAt == nil {
		t.Fatalf("project admin archiving: %d", code)
	}
	if code := h.do("POST", "/projects/"+project.Id.String()+"/archive", gen.ArchiveProject{Archived: false}, nil); code != 200 {
		t.Fatalf("project admin restoring: %d", code)
	}
	if code := h.do("DELETE", "/projects/"+project.Id.String(), nil, nil); code != 403 {
		t.Fatalf("project admin deleting: %d", code)
	}

	// A member of someone else's project may not archive it.
	otherID, other := h.login("member")
	h.member(project.Id.String(), otherID, "member")
	h.token = other
	if code := h.do("POST", "/projects/"+project.Id.String()+"/archive", gen.ArchiveProject{Archived: true}, nil); code != 403 {
		t.Fatalf("a member archiving: %d", code)
	}
	h.do("GET", "/projects/"+project.Id.String(), nil, &project)
	if project.Role == nil || *project.Role != gen.ProjectRoleMember {
		t.Fatalf("a member's role: %+v", project.Role)
	}

	// Runners cannot start projects.
	h.token = admin
	var issued gen.IssuedToken
	h.do("POST", "/tokens", gen.NewToken{Kind: "runner", Name: "r"}, &issued)
	h.token = issued.Token
	if code := h.do("POST", "/projects", gen.NewProject{Slug: slug + "r", Name: "R"}, nil); code != 403 {
		t.Fatalf("a runner creating a project: %d", code)
	}
}

func TestRepositoriesComeAndGo(t *testing.T) {
	h := newHarness(t)
	var project gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("repos%d", time.Now().UnixNano()), Name: "Repos"}, &project)
	base := "/projects/" + project.Id.String() + "/repos"

	if code := h.do("PUT", base, gen.ProjectRepo{Name: "app", Url: "onegator/gator-app"}, nil); code != 200 {
		t.Fatalf("first repository: %d", code)
	}
	if code := h.do("PUT", base, gen.ProjectRepo{Name: "server", Url: "onegator/gator"}, nil); code != 200 {
		t.Fatalf("second repository: %d", code)
	}
	if code := h.do("DELETE", base+"/app", nil, nil); code != 409 {
		t.Fatalf("removing the primary while another remains: %d", code)
	}
	if code := h.do("PUT", base, gen.ProjectRepo{Name: "server", Url: "onegator/gator", Primary: ptr(true)}, nil); code != 200 {
		t.Fatalf("making another primary: %d", code)
	}
	if code := h.do("DELETE", base+"/app", nil, nil); code != 204 {
		t.Fatalf("removing a secondary: %d", code)
	}
	if code := h.do("DELETE", base+"/app", nil, nil); code != 404 {
		t.Fatalf("removing it twice: %d", code)
	}
	var repos []gen.ProjectRepo
	h.do("GET", base, nil, &repos)
	if len(repos) != 1 || repos[0].Name != "server" || !*repos[0].Primary {
		t.Fatalf("what is left: %+v", repos)
	}
}
