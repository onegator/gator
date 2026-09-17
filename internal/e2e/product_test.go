package e2e

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/product"
)

func (h *harness) productEntries(projectID string) []gen.ProductEntry {
	h.t.Helper()
	var out []gen.ProductEntry
	if code := h.do("GET", "/projects/"+projectID+"/product", nil, &out); code != 200 {
		h.t.Fatalf("product: %d", code)
	}
	return out
}

func (h *harness) jobContext(taskID, role string) []string {
	h.t.Helper()
	var id pgtype.UUID
	_ = id.Scan(taskID)
	docs, err := h.svc.JobContext(context.Background(), id, role)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, d := range docs {
		out = append(out, d.Kind+"|"+d.Title+"|"+d.Body)
	}
	return out
}

// What a project knows about its product reaches the roles that weigh direction, and never
// leaves the project.
func TestProductContextInPromptsStaysInItsProject(t *testing.T) {
	h := newHarness(t)
	here, elsewhere := h.task(), h.task()

	var vision gen.ProductEntry
	if code := h.do("POST", "/projects/"+here.ProjectId.String()+"/product",
		gen.NewProductEntry{Kind: "vision", Title: "Why this exists", Content: "People working at night want the app to stop glowing."},
		&vision); code != 201 {
		t.Fatalf("write vision: %d", code)
	}
	if vision.Status != "approved" || vision.Version != 1 {
		t.Fatalf("a person's entry needs no approval: %+v", vision)
	}

	planner := strings.Join(h.jobContext(here.Id.String(), "planner"), "\n")
	if !strings.Contains(planner, "product|vision: Why this exists (v1)") || !strings.Contains(planner, "stop glowing") {
		t.Fatalf("the planner should read the vision:\n%s", planner)
	}
	if worker := strings.Join(h.jobContext(here.Id.String(), "worker"), "\n"); strings.Contains(worker, "product|") {
		t.Fatalf("a worker follows the plan, not the vision:\n%s", worker)
	}
	if other := strings.Join(h.jobContext(elsewhere.Id.String(), "planner"), "\n"); strings.Contains(other, "stop glowing") {
		t.Fatalf("another project must not see it:\n%s", other)
	}
}

// Approving a plan proposes what was decided; a person approves that in turn.
func TestCuratorProposesDecisionsAndLessons(t *testing.T) {
	h := newHarness(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go product.NewCurator(h.pool, h.hub, slog.Default(), 50*time.Millisecond).Run(ctx)
	time.Sleep(150 * time.Millisecond)

	task := h.task() // a bug, in planning
	if _, err := h.pool.Exec(ctx, `INSERT INTO artifacts (task_id, phase, type, version, content) VALUES ($1, 'planning', 'plan', 1, $2)`,
		task.Id.String(), "# Plan\nFix the cache, not the caller."); err != nil {
		t.Fatal(err)
	}
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/approve", nil, nil); code != 200 {
		t.Fatalf("approve: %d", code)
	}

	var decision gen.ProductEntry
	h.waitFor(t, "a proposed decision", func() bool {
		for _, e := range h.productEntries(task.ProjectId.String()) {
			if e.Kind == "decision" && e.SourceTaskId != nil && e.SourceTaskId.String() == task.Id.String() {
				decision = e
				return true
			}
		}
		return false
	})
	if decision.Status != "proposed" || !strings.Contains(decision.Content, "Fix the cache") {
		t.Fatalf("proposal: %+v", decision)
	}
	// A proposal stays out of prompts until a person approves it. The plan itself is in the
	// prompt as an artifact, so look for a product document, not for its words.
	if before := strings.Join(h.jobContext(task.Id.String(), "planner"), "\n"); strings.Contains(before, "product|decision:") {
		t.Fatal("a proposal must not reach a prompt")
	}
	var approved gen.ProductEntry
	if code := h.do("POST", "/product/"+decision.Id.String(), gen.ProductStatusChange{Status: "approved"}, &approved); code != 200 || approved.Status != "approved" {
		t.Fatalf("approve the proposal: %d %+v", code, approved)
	}
	after := strings.Join(h.jobContext(task.Id.String(), "planner"), "\n")
	if !strings.Contains(after, "product|decision: "+task.Title) || !strings.Contains(after, "Fix the cache") {
		t.Fatalf("an approved decision belongs in the prompt:\n%s", after)
	}
}

func (h *harness) waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
