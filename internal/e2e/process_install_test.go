package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/marketplace/marketplacetest"
)

func (h *harness) processConfig(projectID string) map[string]any {
	h.t.Helper()
	var raw []byte
	if err := h.pool.QueryRow(context.Background(), `SELECT process_config FROM projects WHERE id = $1`, projectID).Scan(&raw); err != nil {
		h.t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func phaseNamesOf(t gen.ProjectTemplate) []string {
	var out []string
	for _, p := range t.Phases {
		out = append(out, p.Name)
	}
	return out
}

// A process package sets the process of one kind of task, and the prompts of the roles it
// brings, in a project (or the workspace); the rest of the project's process stays as it was. Its
// removal puts back what it replaced.
func TestAProcessPackageSetsAKindsProcessAndGivesItBack(t *testing.T) {
	h := newHarness(t)
	h.emptyWorkspaceProcessAfterwards()
	p := h.oneProject("proc")
	project := p.Id.String()
	if _, err := h.pool.Exec(context.Background(), `UPDATE projects SET process_config = $2 WHERE id = $1`, project,
		`{"roles":{"postmortem":"Our own words."},"autopilot":{"enabled":false}}`); err != nil {
		t.Fatal(err)
	}
	before := h.template(project, "incident")
	asked := h.accept("process", "incident-postmortem", "")
	inst := h.install(project, gen.NewInstallation{Kind: "process", Name: "incident-postmortem", AcceptedPermissions: asked})

	after := h.template(project, "incident")
	if after.Source == nil || *after.Source != "project" || len(after.Phases) != 4 || after.Phases[2].Name != "postmortem" || after.Phases[2].Role == nil || *after.Phases[2].Role != "postmortem" {
		t.Fatalf("the incident process: %+v", after)
	}
	cfg := h.processConfig(project)
	if cfg["roles"].(map[string]any)["postmortem"] == "Our own words." || cfg["autopilot"] == nil {
		t.Fatalf("the role's prompt is the package's, the rest the project's: %v", cfg)
	}
	var task gen.Task
	if code := h.do("POST", "/projects/"+project+"/tasks", gen.NewTask{Kind: "incident", Title: "down"}, &task); code != 201 || task.Phase != "implementation" {
		t.Fatalf("an incident follows it: %d %+v", code, task)
	}

	if code := h.do("DELETE", "/installations/"+inst.Id.String(), nil, nil); code != 204 {
		t.Fatalf("remove: %d", code)
	}
	back := h.template(project, "incident")
	if *back.Source != *before.Source || len(back.Phases) != len(before.Phases) {
		t.Fatalf("what it replaced comes back: %+v", back)
	}
	cfg = h.processConfig(project)
	if cfg["roles"].(map[string]any)["postmortem"] != "Our own words." || cfg["autopilot"] == nil || cfg["templates"] != nil {
		t.Fatalf("the project's own process is back as it was: %v", cfg)
	}

	// For the workspace: every project without its own gets it.
	other := h.oneProject("proc2")
	var ws gen.Installation
	if code := h.do("POST", "/workspace/installations", gen.NewInstallation{Kind: "process", Name: "incident-postmortem", AcceptedPermissions: asked}, &ws); code != 201 {
		t.Fatalf("workspace install: %d", code)
	}
	if tpl := h.template(other.Id.String(), "incident"); *tpl.Source != "workspace" || len(phaseNamesOf(tpl)) != 4 {
		t.Fatalf("the workspace's incident process: %+v", tpl)
	}
	if code := h.do("DELETE", "/installations/"+ws.Id.String(), nil, nil); code != 204 {
		t.Fatalf("remove from the workspace: %d", code)
	}
	if tpl := h.template(other.Id.String(), "incident"); *tpl.Source != "default" {
		t.Fatalf("after: %+v", tpl)
	}
}

// One kind of task has one process package in a scope: a second is refused, rather than each
// putting back, on removal, what the other replaced. What a person changed after installing is
// theirs, and removal leaves it.
func TestAKindsProcessHasOneOwner(t *testing.T) {
	h := newHarness(t)
	r := marketplacetest.New(t)
	other := map[string]string{}
	for k, v := range marketplacetest.IncidentPostmortem {
		other[k] = strings.ReplaceAll(v, "incident-postmortem", "incident-lite")
	}
	r.Package("process/incident-lite", other)
	r.Publish()
	reg, _ := h.registry(r)
	p := h.oneProject("own")
	project := p.Id.String()
	inst := h.install(project, gen.NewInstallation{Kind: "process", Name: "incident-postmortem", AcceptedPermissions: h.accept("process", "incident-postmortem", "")})
	var refusal gen.InstallRefusal
	if code := h.do("POST", "/projects/"+project+"/installations", gen.NewInstallation{Kind: "process", Name: "incident-lite", RegistryId: &reg.Id,
		AcceptedPermissions: h.accept("process", "incident-lite", reg.Id.String())}, &refusal); code != 409 || !strings.Contains(refusal.Error, "incident-postmortem sets the process of incident") {
		t.Fatalf("a second owner: %d %+v", code, refusal)
	}
	own := `{"templates":{"incident":{"kind":"incident","max_rollbacks":0,"phases":[{"name":"implementation","owner":"runner","role":"worker","gate":"human","timeout":"1h"},{"name":"approved","owner":"human","gate":"auto","timeout":"0"}]}},"roles":{"postmortem":"Ours now."}}`
	if _, err := h.pool.Exec(context.Background(), `UPDATE projects SET process_config = $2 WHERE id = $1`, project, own); err != nil {
		t.Fatal(err)
	}
	if code := h.do("DELETE", "/installations/"+inst.Id.String(), nil, nil); code != 204 {
		t.Fatalf("remove: %d", code)
	}
	if tpl := h.template(project, "incident"); len(tpl.Phases) != 2 {
		t.Fatalf("the person's own template was put back over: %+v", tpl)
	}
	if cfg := h.processConfig(project); cfg["roles"].(map[string]any)["postmortem"] != "Ours now." {
		t.Fatalf("the person's own prompt: %v", cfg)
	}
}
