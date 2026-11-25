package e2e

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
)

// replyJobOf asks an agent something in #general and leases its job to a raw v3 runner, as the
// runner would receive it.
func (h *harness) replyJobOf(project, handle string) (proto.Job, string) {
	h.t.Helper()
	backend := uniqueBackend()
	if _, err := h.pool.Exec(context.Background(), "UPDATE projects SET process_config = $2 WHERE id = $1",
		project, `{"autopilot":{"backend":"`+backend+`"}}`); err != nil {
		h.t.Fatal(err)
	}
	turn := h.turnOf(h.say(project, h.generalOf(project).Id.String(), "@"+handle+" have a look", handle), project, handle)
	if turn.Job == nil {
		h.t.Fatalf("@%s has no job: %+v", handle, turn)
	}
	r, _ := h.dialRaw(h.runner, backend, proto.Version, 1)
	jobs := r.lease(1)
	if len(jobs) != 1 || jobs[0].JobID != *turn.Job {
		h.t.Fatalf("lease: %+v", jobs)
	}
	return jobs[0], *turn.Job
}

// QA Engineer brings e2e-playwright and needs go-testing. Installed, its own skill is the
// project's at once; go-testing is the project's when somebody installs it. A job of @qa carries
// both, whole, in its lease, and reads them through skill_read — only those it carries, and only
// files that are theirs. Nothing of a skill ran when it was installed.
func TestAnAgentsSkillsReachItsJob(t *testing.T) {
	h := newHarness(t)
	p := h.oneProject("skills")
	project := p.Id.String()
	qa := h.install(project, gen.NewInstallation{Kind: "agent", Name: "qa-engineer", AcceptedPermissions: h.accept("agent", "qa-engineer", "")})

	var skills []gen.SkillInfo
	h.do("GET", "/projects/"+project+"/skills", nil, &skills)
	if len(skills) != 1 || skills[0].Name != "e2e-playwright" || skills[0].Source["agent"] != "qa-engineer" ||
		!slices.Contains(skills[0].Files, "scripts/run.sh") {
		t.Fatalf("skills after the agent: %+v", skills)
	}
	var d gen.MarketplaceDetail
	h.do("GET", "/marketplace/agent/qa-engineer?projectId="+project, nil, &d)
	if d.Missing == nil || !slices.Contains(*d.Missing, "skill go-testing") {
		t.Fatalf("go-testing is missing: %v", d.Missing)
	}
	goTesting := h.install(project, gen.NewInstallation{Kind: "skill", Name: "go-testing", AcceptedPermissions: h.accept("skill", "go-testing", "")})
	h.do("GET", "/marketplace/agent/qa-engineer?projectId="+project, nil, &d)
	if d.Missing != nil && slices.Contains(*d.Missing, "skill go-testing") {
		t.Fatalf("installed, it is not missing: %v", *d.Missing)
	}

	job, jobID := h.replyJobOf(project, "qa")
	var names []string
	for _, sk := range job.Skills {
		names = append(names, sk.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"e2e-playwright", "go-testing"}) {
		t.Fatalf("the lease carries: %v", names)
	}
	for _, sk := range job.Skills {
		if sk.Name == "e2e-playwright" && (len(sk.Files) != 2 || sk.Description == "") {
			t.Fatalf("a skill comes whole: %+v", sk)
		}
	}

	cs := h.mcpSession(h.identity(jobID, project))
	var read struct {
		Content string
		Files   []string
	}
	if ok, why := callTool(t, cs, "skill_read", map[string]any{"name": "go-testing"}, &read); !ok || !strings.Contains(read.Content, "never mock SQL") {
		t.Fatalf("skill_read: %s %+v", why, read)
	}
	if ok, why := callTool(t, cs, "skill_read", map[string]any{"name": "e2e-playwright", "path": "scripts/run.sh"}, &read); !ok || !strings.Contains(read.Content, "playwright test") {
		t.Fatalf("a file of a skill: %s", why)
	}
	for _, in := range []map[string]any{{"name": "adr-writing"}, {"name": "go-testing", "path": "../../etc/passwd"}, {"name": "go-testing", "path": "nope.md"}} {
		if ok, _ := callTool(t, cs, "skill_read", in, nil); ok {
			t.Fatalf("%v is not there to read", in)
		}
	}
	// A reviewer carries no skills: there is nothing for it to read.
	reviewer := h.turnOf(h.say(project, h.generalOf(project).Id.String(), "@reviewer look", "reviewer"), project, "reviewer")
	if ok, _ := callTool(t, h.mcpSession(h.identity(*reviewer.Job, project)), "skill_read", map[string]any{"name": "go-testing"}, nil); ok {
		t.Fatal("a skill another agent carries")
	}
	// An admin pins a skill to an agent by name.
	var pinned gen.Agent
	if code := h.do("PATCH", "/agents/"+h.agentID(project, "reviewer").String(), gen.AgentPatch{Skills: &[]string{"go-testing"}}, &pinned); code != 200 ||
		!slices.Equal(pinned.Skills, []string{"go-testing"}) {
		t.Fatalf("pinning: %d %+v", code, pinned.Skills)
	}
	if code := h.do("PATCH", "/agents/"+h.agentID(project, "reviewer").String(), gen.AgentPatch{Skills: &[]string{"../x"}}, nil); code != 400 {
		t.Fatalf("a skill that is not a name: %d", code)
	}
	if ok, why := callTool(t, h.mcpSession(h.identity(*reviewer.Job, project)), "skill_read", map[string]any{"name": "go-testing"}, nil); !ok {
		t.Fatalf("pinned, it reads it: %s", why)
	}
	if n := h.count(`SELECT count(*) FROM agent_calls WHERE job_id = $1 AND command = 'mcp skill_read'`, jobID); n < 5 {
		t.Fatalf("every read is recorded: %d", n)
	}

	// Uninstalling the agent takes away the skill it brought, and leaves the one installed on its own.
	if code := h.do("DELETE", "/installations/"+qa.Id.String(), nil, nil); code != 409 {
		t.Fatalf("an agent still answering stays: %d", code)
	}
	if _, err := h.pool.Exec(context.Background(), `UPDATE agent_turns SET status = 'done' WHERE agent_id = $1`, qa.TargetId.String()); err != nil {
		t.Fatal(err)
	}
	if code := h.do("DELETE", "/installations/"+qa.Id.String(), nil, nil); code != 204 {
		t.Fatalf("uninstall: %d", code)
	}
	h.do("GET", "/projects/"+project+"/skills", nil, &skills)
	if len(skills) != 1 || skills[0].Id != *goTesting.TargetId {
		t.Fatalf("skills after the agent went: %+v", skills)
	}
}
