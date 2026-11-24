package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// member signs in somebody with this role in the project and answers their token.
func (h *harness) member(projectID, role string) string {
	h.t.Helper()
	ctx := context.Background()
	u, err := db.New(h.pool).UpsertUserByEmail(ctx, db.UpsertUserByEmailParams{Email: fmt.Sprintf("m-%d@t.local", time.Now().UnixNano()), Name: role})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, "INSERT INTO memberships(project_id,user_id,role) VALUES($1,$2,$3)", projectID, u.ID, role); err != nil {
		h.t.Fatal(err)
	}
	tok, _, err := auth.Tokens{Pool: h.pool}.Issue(ctx, auth.IssueParams{Kind: auth.KindUser, Scope: "user", Name: "e2e", UserID: u.ID, TTL: time.Hour})
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func handles(list []gen.Agent) map[string]gen.Agent {
	out := map[string]gen.Agent{}
	for _, a := range list {
		out[a.Handle] = a
	}
	return out
}

// The agents of a project as its people see and its admins keep them: every project's defaults,
// its own, the workspace's; a member mentions them, an admin makes and changes them, a built-in
// one is paused rather than deleted, and a paused one says so when asked.
func TestAgentsEndpoints(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	project := task.ProjectId.String()
	member, viewer := h.member(project, "member"), h.member(project, "viewer")

	var list []gen.Agent
	if code := h.asJob(viewer, "GET", "/projects/"+project+"/agents", nil, &list); code != 200 {
		t.Fatalf("a viewer lists agents: %d", code)
	}
	all := handles(list)
	for _, want := range []string{"researcher", "planner", "worker", "reviewer", "security", "curator", "triage", "reporter"} {
		if a, ok := all[want]; !ok || !a.Builtin || a.ProjectId == nil {
			t.Fatalf("@%s: %+v", want, a)
		}
	}
	if all["worker"].Tier != "work" || all["reviewer"].Tier != "chat" || deref(all["security"].RoleKey) != "security_reviewer" {
		t.Fatalf("defaults: %+v", all)
	}

	in := gen.NewAgent{Handle: "backend-reviewer", DisplayName: "Backend reviewer", RoleKey: new("reviewer"),
		Instructions: new("Review the Go code."), Policy: &gen.AgentPolicy{MaxCostUsd: new(1.5)}}
	if code := h.asJob(member, "POST", "/projects/"+project+"/agents", in, nil); code != 403 {
		t.Fatalf("a member made an agent: %d", code)
	}
	var custom gen.Agent
	if code := h.do("POST", "/projects/"+project+"/agents", in, &custom); code != 201 || custom.Builtin || custom.Tier != "chat" ||
		custom.Policy.MaxCostUsd == nil || *custom.Policy.MaxCostUsd != 1.5 {
		t.Fatalf("create: %d %+v", code, custom)
	}
	if code := h.do("POST", "/projects/"+project+"/agents", in, nil); code != 409 {
		t.Fatalf("the same handle twice: %d", code)
	}
	if code := h.do("POST", "/projects/"+project+"/agents", gen.NewAgent{Handle: "Bad Handle", DisplayName: "x"}, nil); code != 400 {
		t.Fatalf("a bad handle: %d", code)
	}
	// A workspace agent cannot take a handle any project has, and every project sees it.
	if code := h.do("POST", "/workspace/agents", gen.NewAgent{Handle: "reviewer", DisplayName: "R"}, nil); code != 409 {
		t.Fatalf("a workspace @reviewer: %d", code)
	}
	ws := gen.NewAgent{Handle: fmt.Sprintf("helper-%d", time.Now().UnixNano()%1_000_000_000), DisplayName: "Helper"}
	if code := h.asJob(member, "POST", "/workspace/agents", ws, nil); code != 403 {
		t.Fatalf("a member made a workspace agent: %d", code)
	}
	var helper gen.Agent
	if code := h.do("POST", "/workspace/agents", ws, &helper); code != 201 || helper.ProjectId != nil {
		t.Fatalf("workspace agent: %d %+v", code, helper)
	}
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), "DELETE FROM agents WHERE id = $1", helper.Id.String())
	})
	h.asJob(member, "GET", "/projects/"+project+"/agents", nil, &list)
	if _, ok := handles(list)[ws.Handle]; !ok {
		t.Fatal("a project sees the workspace's agents")
	}

	// A built-in agent keeps its handle, is paused rather than deleted, and a paused agent says so.
	reviewer := all["reviewer"].Id.String()
	if code := h.do("PATCH", "/agents/"+reviewer, gen.AgentPatch{Handle: new("critic")}, nil); code != 400 {
		t.Fatalf("renamed a built-in agent: %d", code)
	}
	if code := h.do("DELETE", "/agents/"+reviewer, nil, nil); code != 409 {
		t.Fatalf("deleted a built-in agent: %d", code)
	}
	paused := gen.AgentPatchStatus("paused")
	var after gen.Agent
	if code := h.asJob(member, "PATCH", "/agents/"+reviewer, gen.AgentPatch{Status: &paused}, nil); code != 403 {
		t.Fatalf("a member paused an agent: %d", code)
	}
	if code := h.do("PATCH", "/agents/"+reviewer, gen.AgentPatch{Status: &paused}, &after); code != 200 || after.Status != "paused" {
		t.Fatalf("pause: %d %+v", code, after)
	}
	general := h.generalOf(project)
	ask := h.say(project, general.Id.String(), "@reviewer are you there?", "reviewer")
	refused := h.turnOf(ask, project, "reviewer")
	if refused.Status != "refused" || deref(refused.Refusal) != "paused" {
		t.Fatalf("a paused agent's turn: %+v", refused)
	}

	// The profile and the history of the agent that answered.
	var profile gen.AgentProfile
	if code := h.asJob(viewer, "GET", "/agents/"+reviewer, nil, &profile); code != 200 || profile.Stats.Turns != 1 || profile.Stats.Refused != 1 {
		t.Fatalf("profile: %d %+v", code, profile.Stats)
	}
	var turns []gen.AgentTurn
	if code := h.asJob(viewer, "GET", "/agents/"+reviewer+"/turns", nil, &turns); code != 200 || len(turns) != 1 || turns[0].Refusal == nil {
		t.Fatalf("turns: %d %+v", code, turns)
	}
	stranger := h.person()
	if code := h.asJob(stranger, "GET", "/agents/"+reviewer, nil, nil); code != 404 {
		t.Fatalf("somebody outside the project read its agent: %d", code)
	}

	if code := h.do("DELETE", "/agents/"+custom.Id.String(), nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code := h.do("GET", "/agents/"+custom.Id.String(), nil, nil); code != 404 {
		t.Fatalf("deleted agent: %d", code)
	}

	// Automations say whose work they are.
	var autos []gen.Automation
	h.do("GET", "/projects/"+project+"/automations", nil, &autos)
	for _, a := range autos {
		if a.Key == "error_triage" && deref(a.Author) != "triage" {
			t.Fatalf("error triage is @triage's: %+v", a)
		}
	}
}

// What is said to an agent in a DM stays in the DM: the ask behind it, the job's instruction and
// the record of what it was handed only point at it, and someone in the project but not in the DM
// sees no trace of the job.
func TestAnAgentInADMKeepsItPrivate(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	project := task.ProjectId.String()
	backend := uniqueBackend()
	if _, err := h.pool.Exec(context.Background(), "UPDATE projects SET process_config = $2 WHERE id = $1", project, `{"autopilot":{"backend":"`+backend+`"}}`); err != nil {
		t.Fatal(err)
	}
	other := h.member(project, "member")
	pid := task.ProjectId
	agents := []openapi_types.UUID{h.agentID(project, "reviewer")}
	var dm gen.ConversationDetail
	if code := h.do("POST", "/conversations/dm", gen.NewDirectMessage{Members: []openapi_types.UUID{}, ProjectId: &pid, Agents: &agents}, &dm); code != 201 {
		t.Fatalf("dm: %d", code)
	}
	secret := "the acquisition of Acme closes on Friday"
	ask := h.say(project, dm.Id.String(), secret)
	turn := h.turnOf(ask, project, "reviewer")
	exec := &tokenExec{seen: make(chan struct{}, 1), hold: true}
	h.startRunner(backend, exec)
	h.waitJob(*turn.Job, "running")

	var job gen.Job
	h.do("GET", "/jobs/"+*turn.Job, nil, &job)
	var askTask gen.TaskDetail
	h.do("GET", "/tasks/"+job.TaskId.String(), nil, &askTask)
	if contains(job.Instruction, "Acme") || contains(askTask.Title, "Acme") || contains(deref(askTask.Description), "Acme") {
		t.Fatalf("private words in the job or its task: %q / %q / %q", job.Instruction, askTask.Title, deref(askTask.Description))
	}
	var pkg gen.ContextPackage
	h.do("GET", "/jobs/"+*turn.Job+"/context", nil, &pkg)
	for _, d := range pkg.Documents {
		if contains(deref(d.Body), "Acme") {
			t.Fatalf("the job's record keeps private words: %+v", d)
		}
	}
	_, leased := exec.token(t)
	if leased.Turn == nil || !contains(leased.Turn.Ask, "Acme") {
		t.Fatal("the agent itself is told what was asked")
	}
	if code := h.asJob(other, "GET", "/jobs/"+*turn.Job, nil, nil); code != 404 {
		t.Fatalf("someone outside the DM read its job: %d", code)
	}
	if code := h.asJob(other, "GET", "/jobs/"+*turn.Job+"/events", nil, nil); code != 404 {
		t.Fatalf("someone outside the DM read its job's output: %d", code)
	}
	var jobs []gen.Job
	h.asJob(other, "GET", "/tasks/"+job.TaskId.String()+"/jobs", nil, &jobs)
	if len(jobs) != 0 {
		t.Fatalf("someone outside the DM listed its job: %+v", jobs)
	}
}
