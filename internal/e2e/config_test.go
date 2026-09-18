package e2e

import (
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// A project's process is readable and writable, and a configuration that would not hold is
// refused rather than stored.
func TestProjectConfigAndPeople(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	project := task.ProjectId.String()

	var config gen.ProjectConfig
	if code := h.do("GET", "/projects/"+project+"/config", nil, &config); code != 200 {
		t.Fatalf("read config: %d", code)
	}

	good := map[string]interface{}{
		"autopilot": map[string]interface{}{"enabled": false},
		"policy":    map[string]interface{}{"default": map[string]interface{}{"backend": "claude"}, "daily_budget_usd": 25},
	}
	var saved gen.ProjectConfig
	if code := h.do("PUT", "/projects/"+project+"/config", gen.ProjectConfig{Config: good}, &saved); code != 200 {
		t.Fatalf("write config: %d", code)
	}
	policy, ok := saved.Config["policy"].(map[string]interface{})
	if !ok || policy["daily_budget_usd"] != float64(25) {
		t.Fatalf("saved config: %+v", saved.Config)
	}
	var reread gen.ProjectConfig
	h.do("GET", "/projects/"+project+"/config", nil, &reread)
	if _, ok := reread.Config["autopilot"]; !ok {
		t.Fatalf("what was written should come back: %+v", reread.Config)
	}

	// A template without phases would leave tasks with nowhere to go.
	broken := map[string]interface{}{"templates": map[string]interface{}{"bug": map[string]interface{}{"kind": "bug"}}}
	var failure gen.Error
	if code := h.do("PUT", "/projects/"+project+"/config", gen.ProjectConfig{Config: broken}, &failure); code != 400 {
		t.Fatalf("a template with no phases must be refused: %d", code)
	}
	if !strings.Contains(failure.Error, "template bug") {
		t.Fatalf("the refusal should name the template: %q", failure.Error)
	}
	var after gen.ProjectConfig
	h.do("GET", "/projects/"+project+"/config", nil, &after)
	if _, ok := after.Config["templates"]; ok {
		t.Fatal("a refused configuration must not be stored")
	}

	// People: whoever creates a project administers it from the start.
	var me gen.Me
	h.do("GET", "/auth/me", nil, &me)
	var members []gen.ProjectMember
	if code := h.do("GET", "/projects/"+project+"/members", nil, &members); code != 200 {
		t.Fatalf("members: %d", code)
	}
	if len(members) != 1 || members[0].UserId != *me.UserId || members[0].Role != gen.ProjectMemberRoleAdmin {
		t.Fatalf("the creator administers the project: %+v", members)
	}
	// A role can be changed, and the list follows.
	if code := h.do("PUT", "/projects/"+project+"/members", gen.Member{UserId: *me.UserId, Role: gen.MemberRoleViewer}, nil); code != 204 {
		t.Fatalf("change a role: %d", code)
	}
	h.do("GET", "/projects/"+project+"/members", nil, &members)
	if len(members) != 1 || members[0].Role != gen.ProjectMemberRoleViewer {
		t.Fatalf("after the change: %+v", members)
	}

	var users []gen.User
	if code := h.do("GET", "/users", nil, &users); code != 200 || len(users) == 0 {
		t.Fatalf("users: %d %+v", code, users)
	}
	found := false
	for _, u := range users {
		found = found || u.Id == *me.UserId
	}
	if !found {
		t.Fatal("the signed-in person is one of the workspace's people")
	}
}
