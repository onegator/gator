package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

// campaign creates a project and a campaign task in its Planning phase.
func (h *harness) campaign(title string) (gen.Project, gen.Task) {
	h.t.Helper()
	var p gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{
		Slug: fmt.Sprintf("c%d", time.Now().UnixNano()), Name: "Fleet"}, &p); code != 201 {
		h.t.Fatalf("project: %d", code)
	}
	desc := "Bump the logging library everywhere."
	var task gen.Task
	if code := h.do("POST", "/projects/"+p.Id.String()+"/tasks",
		gen.NewTask{Kind: gen.NewTaskKindCampaign, Title: title, Description: &desc}, &task); code != 201 {
		h.t.Fatalf("campaign: %d", code)
	}
	return p, task
}

func (h *harness) addTargets(taskID string, keys ...string) gen.Campaign {
	h.t.Helper()
	in := gen.CampaignTargets{Instruction: ptr("Update to 2.0 and run the tests.")}
	for _, k := range keys {
		in.Targets = append(in.Targets, gen.CampaignTargetInput{Key: k})
	}
	var out gen.Campaign
	if code := h.do("POST", "/tasks/"+taskID+"/campaign", in, &out); code != 200 {
		h.t.Fatalf("add targets: %d", code)
	}
	return out
}

// One decision, many places: each target gets its own task, and each moves on its own.
func TestACampaignGivesEveryTargetItsOwnTask(t *testing.T) {
	h := newHarness(t)
	p, campaign := h.campaign("Bump logging to 2.0")
	got := h.addTargets(campaign.Id.String(), "api", "web", "worker")

	if len(got.Targets) != 3 || got.Waiting != 3 {
		t.Fatalf("campaign = %+v", got)
	}
	var tasks []gen.Task
	h.do("GET", "/projects/"+p.Id.String()+"/tasks", nil, &tasks)
	children := 0
	for _, task := range tasks {
		if task.Kind == "chore" {
			children++
		}
	}
	if children != 3 {
		t.Fatalf("three targets should be three tasks, got %d", children)
	}
	// Asking twice must not double the fleet: half a fleet change is the worst outcome of all.
	again := h.addTargets(campaign.Id.String(), "api", "web", "worker")
	if len(again.Targets) != 3 {
		t.Fatalf("adding the same targets again changed the campaign: %+v", again)
	}
}

// One awkward repository must not hold up the other nine.
func TestBlockingOneTargetLeavesTheRestAlone(t *testing.T) {
	h := newHarness(t)
	_, campaign := h.campaign("Drop the old endpoint")
	got := h.addTargets(campaign.Id.String(), "api", "web")

	// Block one target the way a phase timeout would.
	var blocked gen.Task
	for _, target := range got.Targets {
		if target.Key == "api" {
			h.do("GET", "/tasks/"+target.TaskId.String(), nil, &blocked)
		}
	}
	if _, err := h.pool.Exec(t.Context(),
		"UPDATE tasks SET blocked_reason = 'the build has been red for a week' WHERE id = $1",
		blocked.Id.String()); err != nil {
		t.Fatal(err)
	}

	var after gen.Campaign
	h.do("GET", "/tasks/"+campaign.Id.String()+"/campaign", nil, &after)
	if after.Blocked != 1 || after.Waiting != 1 {
		t.Fatalf("one blocked target should leave the other waiting: %+v", after)
	}

	// The campaign is one decision, so the inbox asks about it once — but a stuck target is
	// exactly the thing a person has to see.
	reasons := map[string]string{}
	for _, d := range h.decisions("") {
		reasons[d.Task.Id.String()] = string(d.Reason)
	}
	if reasons[blocked.Id.String()] != "blocked" {
		t.Errorf("a blocked target belongs in the inbox: %v", reasons)
	}
	for _, target := range after.Targets {
		if target.Key == "web" && reasons[target.TaskId.String()] != "" {
			t.Errorf("a target that is simply in progress should not ask for a decision: %v", reasons)
		}
	}
}

// Approving a campaign needs every target finished or explicitly skipped with a reason.
func TestACampaignIsNotDoneUntilEveryTargetIsAnsweredFor(t *testing.T) {
	h := newHarness(t)
	_, campaign := h.campaign("Bump the dependency")
	got := h.addTargets(campaign.Id.String(), "api", "web")

	// Planning is a human phase; approving moves the campaign into Execution.
	h.do("POST", "/tasks/"+campaign.Id.String()+"/approve", nil, nil)
	var moved gen.Task
	if code := h.do("POST", "/tasks/"+campaign.Id.String()+"/advance", nil, &moved); code != 200 {
		t.Fatalf("advance to execution: %d", code)
	}
	if moved.Phase != "execution" {
		t.Fatalf("phase = %s", moved.Phase)
	}

	// With targets outstanding the gate refuses, however much a person approves.
	h.do("POST", "/tasks/"+campaign.Id.String()+"/approve", nil, nil)
	if code := h.do("POST", "/tasks/"+campaign.Id.String()+"/advance", nil, nil); code == 200 {
		t.Fatal("a campaign with unfinished targets should not leave Execution")
	}

	// Finish one target, skip the other with a reason.
	for _, target := range got.Targets {
		if target.Key == "api" {
			h.closeTask(target.TaskId.String())
		}
	}
	var skipped gen.Campaign
	if code := h.do("POST", "/tasks/"+campaign.Id.String()+"/campaign/skip",
		gen.SkipTarget{Key: "web", Reason: "that service is being retired next month"}, &skipped); code != 200 {
		t.Fatalf("skip: %d", code)
	}
	if skipped.Done != 1 || skipped.Skipped != 1 {
		t.Fatalf("campaign = %+v", skipped)
	}
	// A skip without a reason is how the hard half of a fleet change disappears.
	if code := h.do("POST", "/tasks/"+campaign.Id.String()+"/campaign/skip",
		gen.SkipTarget{Key: "api", Reason: ""}, nil); code == 200 {
		t.Error("skipping without a reason should be refused")
	}

	h.do("POST", "/tasks/"+campaign.Id.String()+"/approve", nil, nil)
	if code := h.do("POST", "/tasks/"+campaign.Id.String()+"/advance", nil, &moved); code != 200 {
		t.Fatalf("with every target answered for, the campaign should move on: %d", code)
	}
	if moved.Phase != "verification" {
		t.Fatalf("phase = %s", moved.Phase)
	}
}

// closeTask walks a task to the end the way a person would.
func (h *harness) closeTask(id string) {
	h.t.Helper()
	for range 8 {
		var task gen.Task
		if h.do("GET", "/tasks/"+id, nil, &task); task.ClosedAt != nil {
			return
		}
		h.do("POST", "/tasks/"+id+"/approve", nil, nil)
		if code := h.do("POST", "/tasks/"+id+"/advance", nil, nil); code != 200 {
			h.t.Fatalf("advancing %s: %d", id, code)
		}
	}
}
