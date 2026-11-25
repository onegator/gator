package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/marketplace/marketplacetest"
)

// helper is an agent package; an extra starting "  - " is one more permission, any other a line
// of agent.yaml.
func helper(version, instructions string, extra ...string) map[string]string {
	top, perms := "", "permissions:\n  - reads merged pull requests\n"
	for _, e := range extra {
		if strings.HasPrefix(e, "  - ") {
			perms += e
		} else {
			top += e
		}
	}
	return map[string]string{
		"agent.yaml": "name: release-helper\nversion: " + version + "\ndisplay_name: Release Helper\nhandle: releaser\n" +
			"description: Writes the changelog of release " + version + ".\ntier: chat\n" + top + perms,
		"instructions.md": instructions,
	}
}

// A new version never installs itself. The catalogue says an update waits; the person sees what it
// would change, accepts what it asks for that was never accepted, and says what becomes of what
// they changed themselves — kept, or taken over — before anything moves.
func TestUpdatesWaitForAPersonAndForWhatTheyChanged(t *testing.T) {
	h := newHarness(t)
	r := marketplacetest.New(t)
	r.Package("agents/release-helper", helper("1.0.0", "Write the changelog.\nGroup it by kind.\n"))
	r.Publish()
	reg, _ := h.registry(r)
	p := h.oneProject("upd")
	project := p.Id.String()
	inst := h.install(project, gen.NewInstallation{Kind: "agent", Name: "release-helper", RegistryId: reg.Id, AcceptedPermissions: h.accept("agent", "release-helper", reg.Id.String())})
	agent := inst.Agent.Id.String()

	// 1.1.0 is published and fetched: offered, not installed.
	r.Package("agents/release-helper", helper("1.1.0", "Write the changelog.\nGroup it by kind.\nLink every pull request.\n"))
	r.Publish()
	h.fetch(reg.Id.String(), 200)
	var got gen.Installation
	h.do("GET", "/installations/"+inst.Id.String(), nil, &got)
	if got.Version != "1.0.0" || got.UpdateAvailable == nil || *got.UpdateAvailable != "1.1.0" || strings.Contains(got.Agent.Instructions, "Link every") {
		t.Fatalf("an update waits, nothing moved: %+v", got)
	}
	var listed []gen.MarketplaceItem
	h.do("GET", "/marketplace?kind=agent&q=changelog&projectId="+project, nil, &listed)
	if len(listed) != 1 || listed[0].Installed == nil || !listed[0].Installed.UpdateAvailable {
		t.Fatalf("the Update chip: %+v", listed)
	}
	var pv gen.UpdatePreview
	h.do("GET", "/installations/"+inst.Id.String()+"/update", nil, &pv)
	if pv.From != "1.0.0" || pv.To != "1.1.0" || pv.NeedsAcceptance || len(pv.LocalChanges) != 0 {
		t.Fatalf("preview: %+v", pv)
	}
	if code := h.do("PATCH", "/installations/"+inst.Id.String(), h.pinUpdate(inst.Id.String(), gen.InstallationUpdate{}), &got); code != 200 || got.Version != "1.1.0" ||
		!strings.Contains(got.Agent.Instructions, "Link every") || got.Agent.Handle != "releaser" {
		t.Fatalf("update: %d %+v", code, got)
	}

	// 2.0.0 asks for more: it waits until that is accepted.
	more := "  - publishes the release notes\n"
	r.Package("agents/release-helper", helper("2.0.0", "Write the changelog.\nGroup it by kind.\nLink every pull request.\nThank who helped.\n", more))
	r.Publish()
	h.fetch(reg.Id.String(), 200)
	h.do("GET", "/installations/"+inst.Id.String()+"/update", nil, &pv)
	if !pv.NeedsAcceptance || !slices.Equal(pv.AddedPermissions, []string{"publishes the release notes"}) {
		t.Fatalf("more permissions: %+v", pv)
	}
	var refusal gen.InstallRefusal
	if code := h.do("PATCH", "/installations/"+inst.Id.String(), h.pinUpdate(inst.Id.String(), gen.InstallationUpdate{}), &refusal); code != 409 || refusal.Code != "permissions_not_accepted" ||
		refusal.Permissions == nil || !slices.Equal(*refusal.Permissions, []string{"publishes the release notes"}) {
		t.Fatalf("not accepted: %d %+v", code, refusal)
	}

	// Somebody makes the instructions their own: the update shows both changes and waits.
	own := "Write the changelog.\nOur team writes it in Polish.\nLink every pull request.\n"
	if code := h.do("PATCH", "/agents/"+agent, gen.AgentPatch{Instructions: &own}, nil); code != 200 {
		t.Fatalf("a local edit: %d", code)
	}
	accept := []string{"publishes the release notes"}
	if code := h.do("PATCH", "/installations/"+inst.Id.String(), h.pinUpdate(inst.Id.String(), gen.InstallationUpdate{AcceptedPermissions: &accept}), &refusal); code != 409 ||
		refusal.Code != "local_changes" || refusal.LocalChanges == nil || len(*refusal.LocalChanges) != 1 {
		t.Fatalf("a local edit blocks it: %d %+v", code, refusal)
	}
	change := (*refusal.LocalChanges)[0]
	if change.Part != "instructions" || !strings.Contains(change.Local, "+Our team writes it in Polish.") || !strings.Contains(change.Local, "-Group it by kind.") ||
		!strings.Contains(change.Incoming, "+Thank who helped.") {
		t.Fatalf("the diffs:\n%s\n%s", change.Local, change.Incoming)
	}
	h.do("GET", "/installations/"+inst.Id.String(), nil, &got)
	if got.Version != "1.1.0" || got.Agent.Instructions != own {
		t.Fatalf("nothing moved: %+v", got)
	}

	keep := gen.KeepLocal
	if code := h.do("PATCH", "/installations/"+inst.Id.String(), h.pinUpdate(inst.Id.String(), gen.InstallationUpdate{AcceptedPermissions: &accept, Resolve: &keep}), &got); code != 200 ||
		got.Version != "2.0.0" || got.Agent.Instructions != own || !strings.Contains(got.Agent.Description, "2.0.0") ||
		!slices.Contains(got.AcceptedPermissions, "publishes the release notes") {
		t.Fatalf("kept: %d %+v", code, got)
	}

	// And at the next version the person takes theirs.
	r.Package("agents/release-helper", helper("2.1.0", "Write the changelog, shorter.\n", more))
	r.Publish()
	h.fetch(reg.Id.String(), 200)
	theirs := gen.TakeTheirs
	if code := h.do("PATCH", "/installations/"+inst.Id.String(), h.pinUpdate(inst.Id.String(), gen.InstallationUpdate{}), &refusal); code != 409 || refusal.Code != "local_changes" {
		t.Fatalf("still their own: %d %+v", code, refusal)
	}
	if code := h.do("PATCH", "/installations/"+inst.Id.String(), h.pinUpdate(inst.Id.String(), gen.InstallationUpdate{Resolve: &theirs}), &got); code != 200 ||
		got.Version != "2.1.0" || got.Agent.Instructions != "Write the changelog, shorter.\n" {
		t.Fatalf("taken: %d %+v", code, got)
	}
	if code := h.do("PATCH", "/installations/"+inst.Id.String(), h.pinUpdate(inst.Id.String(), gen.InstallationUpdate{}), &refusal); code != 409 {
		t.Fatalf("installed already: %d", code)
	}
	if code := h.asJob(h.member(project, "member"), "PATCH", "/installations/"+inst.Id.String(), h.pinUpdate(inst.Id.String(), gen.InstallationUpdate{Version: "2.0.0", Checksum: "sha256:any"}), nil); code != 403 {
		t.Fatalf("a member updates: %d", code)
	}
}

// An update that only drops a cost cap is asking for more, not less: it waits for acceptance.
func TestDroppingACostCapIsAcceptedAgain(t *testing.T) {
	h := newHarness(t)
	r := marketplacetest.New(t)
	r.Package("agents/release-helper", helper("1.0.0", "Write the changelog.\n", "policy: {max_cost_usd: 2, daily_budget_usd: 10}\n"))
	r.Publish()
	reg, _ := h.registry(r)
	p := h.oneProject("cap")
	inst := h.install(p.Id.String(), gen.NewInstallation{Kind: "agent", Name: "release-helper", RegistryId: reg.Id, AcceptedPermissions: h.accept("agent", "release-helper", reg.Id.String())})
	r.Package("agents/release-helper", helper("1.0.1", "Write the changelog.\n"))
	r.Publish()
	h.fetch(reg.Id.String(), 200)
	var pv gen.UpdatePreview
	h.do("GET", "/installations/"+inst.Id.String()+"/update", nil, &pv)
	want := []string{"spends what the project's budget allows a day (no daily cap of its own)", "spends what the project's policy allows a turn (no cap of its own)"}
	if !pv.NeedsAcceptance || !slices.Equal(pv.AddedPermissions, want) {
		t.Fatalf("dropping the caps: %+v", pv)
	}
	var refusal gen.InstallRefusal
	if code := h.do("PATCH", "/installations/"+inst.Id.String(), h.pinUpdate(inst.Id.String(), gen.InstallationUpdate{}), &refusal); code != 409 || refusal.Code != "permissions_not_accepted" {
		t.Fatalf("applied without acceptance: %d %+v", code, refusal)
	}
}

// What a person changed of an installed agent beyond its instructions — its tools, skills, tier,
// policy, role — is a local change too: an update shows it and waits. A skill the agent brought
// and its new version no longer does goes with the update.
func TestAnAgentsOwnSettingsAreLocalChanges(t *testing.T) {
	h := newHarness(t)
	r := marketplacetest.New(t)
	pkg := helper("1.0.0", "Write the changelog.\n", "skills: [changelog-style]\n")
	pkg["skills/changelog-style/SKILL.md"] = "---\nname: changelog-style\ndescription: How our changelog reads.\n---\nKeep a Changelog.\n"
	r.Package("agents/release-helper", pkg)
	r.Publish()
	reg, _ := h.registry(r)
	p := h.oneProject("own")
	project := p.Id.String()
	inst := h.install(project, gen.NewInstallation{Kind: "agent", Name: "release-helper", RegistryId: reg.Id, AcceptedPermissions: h.accept("agent", "release-helper", reg.Id.String())})
	if code := h.do("PATCH", "/agents/"+inst.Agent.Id.String(), gen.AgentPatch{Tools: &[]string{"issues-mcp"}, Tier: ptr(gen.AgentPatchTierWork)}, nil); code != 200 {
		t.Fatalf("local changes: %d", code)
	}

	r.Write("agents/release-helper/agent.yaml", helper("1.1.0", "")["agent.yaml"])
	_ = os.RemoveAll(filepath.Join(r.Dir, "agents/release-helper/skills"))
	r.Publish()
	h.fetch(reg.Id.String(), 200)
	var pv gen.UpdatePreview
	h.do("GET", "/installations/"+inst.Id.String()+"/update", nil, &pv)
	var parts []string
	for _, c := range pv.LocalChanges {
		parts = append(parts, c.Part)
	}
	if !slices.Equal(parts, []string{"tier", "tools"}) {
		t.Fatalf("local changes: %v", parts)
	}
	var refusal gen.InstallRefusal
	if code := h.do("PATCH", "/installations/"+inst.Id.String(), h.pinUpdate(inst.Id.String(), gen.InstallationUpdate{AcceptedPermissions: &pv.AddedPermissions}), &refusal); code != 409 || refusal.Code != "local_changes" {
		t.Fatalf("overwritten without a word: %d %+v", code, refusal)
	}
	keep := gen.KeepLocal
	var got gen.Installation
	if code := h.do("PATCH", "/installations/"+inst.Id.String(), h.pinUpdate(inst.Id.String(), gen.InstallationUpdate{AcceptedPermissions: &pv.AddedPermissions, Resolve: &keep}), &got); code != 200 {
		t.Fatalf("keep: %d", code)
	}
	if got.Agent.Tier != "work" || !slices.Equal(got.Agent.Tools, []string{"issues-mcp"}) || len(got.Agent.Skills) != 0 {
		t.Fatalf("kept, and the dropped skill not carried: %+v", got.Agent)
	}
	var skills []gen.SkillInfo
	h.do("GET", "/projects/"+project+"/skills", nil, &skills)
	if len(skills) != 0 {
		t.Fatalf("the skill the new version dropped stayed: %+v", skills)
	}
}
