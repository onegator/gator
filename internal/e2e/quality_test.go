package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

// qualityProject makes a project whose scorecard is on, with one component.
func (h *harness) qualityProject(policy map[string]any, component gen.ComponentInput) gen.Project {
	h.t.Helper()
	var p gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{
		Slug: fmt.Sprintf("q%d", time.Now().UnixNano()), Name: "Quality"}, &p); code != 201 {
		h.t.Fatalf("project: %d", code)
	}
	cfg, _ := json.Marshal(map[string]any{"quality": policy})
	if _, err := h.pool.Exec(context.Background(),
		"UPDATE projects SET process_config = $2 WHERE id = $1", p.Id.String(), string(cfg)); err != nil {
		h.t.Fatal(err)
	}
	h.putComponent(p.Id.String(), component)
	return p
}

func (h *harness) scorecards(projectID string) []gen.Scorecard {
	h.t.Helper()
	var out []gen.Scorecard
	if code := h.do("GET", "/projects/"+projectID+"/quality", nil, &out); code != 200 {
		h.t.Fatalf("quality: %d", code)
	}
	return out
}

// The rule the whole feature stands on: a component that has slipped gets one task, not one per
// sweep, and the task closes by itself once the rule passes again.
func TestASlipOpensOneTaskAndClosesItselfWhenFixed(t *testing.T) {
	h := newHarness(t)
	// A component with no owner, no repository and no notes fails everything the core can ask.
	p := h.qualityProject(map[string]any{"enabled": true},
		gen.ComponentInput{Key: "api", Name: "API", Kind: "api"})

	for range 3 {
		if err := h.quality.Sweep(context.Background(), projectUUID(p.Id.String())); err != nil {
			t.Fatal(err)
		}
	}
	var chores []gen.Task
	var tasks []gen.Task
	h.do("GET", "/projects/"+p.Id.String()+"/tasks", nil, &tasks)
	for _, task := range tasks {
		if task.Kind == "chore" {
			chores = append(chores, task)
		}
	}
	if len(chores) != 1 {
		t.Fatalf("three sweeps of one slip should be one task, got %d", len(chores))
	}
	if chores[0].ClosedAt != nil {
		t.Fatal("the task should be open while the rules fail")
	}

	cards := h.scorecards(p.Id.String())
	if len(cards) != 1 || cards[0].Component != "api" || cards[0].Earned != 0 {
		t.Fatalf("scorecard = %+v", cards)
	}
	if cards[0].Possible != 5 { // owner 2 + repo 2 + notes 1
		t.Errorf("possible = %d", cards[0].Possible)
	}

	// Now the component is filled in, which is exactly what the task asked for.
	h.putComponent(p.Id.String(), gen.ComponentInput{Key: "api", Name: "API", Kind: "api",
		Repo: ptr("acme/api"), Notes: ptr("Talks to the payments provider.")})
	if err := h.quality.Sweep(context.Background(), projectUUID(p.Id.String())); err != nil {
		t.Fatal(err)
	}

	var after gen.Task
	h.do("GET", "/tasks/"+chores[0].Id.String(), nil, &after)
	if after.ClosedAt == nil {
		t.Fatal("a rule that passes again should close the task it opened")
	}
	cards = h.scorecards(p.Id.String())
	if cards[0].Earned != 3 || cards[0].PreviousEarned == nil || *cards[0].PreviousEarned != 0 {
		t.Fatalf("the trend should show the improvement: %+v", cards[0])
	}
}

// A scorecard nobody asked for is noise.
func TestScoringIsOffUnlessAProjectAsksForIt(t *testing.T) {
	h := newHarness(t)
	p := h.qualityProject(map[string]any{"enabled": false},
		gen.ComponentInput{Key: "web", Name: "Web", Kind: "app"})
	if err := h.quality.Sweep(context.Background(), projectUUID(p.Id.String())); err != nil {
		t.Fatal(err)
	}
	if cards := h.scorecards(p.Id.String()); len(cards) != 1 || cards[0].Possible != 0 {
		t.Fatalf("nothing should have been scored: %+v", cards)
	}
	if h.count("SELECT count(*) FROM tasks WHERE project_id = $1", p.Id.String()) != 0 {
		t.Fatal("a project that did not ask for a scorecard should get no tasks from one")
	}
}

// Running it by hand is for the person who has just fixed something.
func TestScoringOnDemandAnswersTheNewScore(t *testing.T) {
	h := newHarness(t)
	p := h.qualityProject(map[string]any{"enabled": true, "on_regression": "none"},
		gen.ComponentInput{Key: "lib", Name: "Lib", Kind: "lib", Repo: ptr("acme/lib")})

	var cards []gen.Scorecard
	if code := h.do("POST", "/projects/"+p.Id.String()+"/quality", nil, &cards); code != 200 {
		h.t.Fatalf("score now: %d", code)
	}
	if len(cards) != 1 || cards[0].Earned != 2 || cards[0].Possible != 5 {
		t.Fatalf("scorecard = %+v", cards)
	}
	if h.count("SELECT count(*) FROM tasks WHERE project_id = $1", p.Id.String()) != 0 {
		t.Fatal("on_regression none means numbers without tasks")
	}
}
