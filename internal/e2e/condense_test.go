package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/product"
)

// seedLessons puts approved lessons into a project the way months of work would.
func (h *harness) seedLessons(projectID string, n int) {
	h.t.Helper()
	for i := range n {
		if _, err := h.pool.Exec(context.Background(),
			`INSERT INTO product_context (project_id, kind, title, content, version, status, created_by_kind, approved_at)
			 VALUES ($1, 'lesson', $2, $3, 1, 'approved', 'system', now())`,
			projectID, fmt.Sprintf("Lesson %d", i), fmt.Sprintf("The cache was stale in case %d, and nobody noticed for a week.", i)); err != nil {
			h.t.Fatal(err)
		}
	}
}

// A project that has learned more than a prompt can carry gets one curation task, and what the
// curator writes becomes one proposal — while every original stays readable.
func TestCondensingProposesOneEntryAndKeepsTheOriginals(t *testing.T) {
	h := newHarness(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	curator := product.NewCurator(h.pool, h.hub, slog.Default(), 50*time.Millisecond)
	condenser := product.NewCondenser(curator, h.svc, 5, 1<<20)
	go curator.Run(ctx)

	p := h.oneProject("cd")
	h.seedLessons(p.Id.String(), 6)

	opened, err := condenser.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if opened != 1 {
		t.Fatalf("one kind over the threshold should open one curation task, got %d", opened)
	}
	// Asking again while the curation is open must not queue a second one.
	if again, err := condenser.Sweep(ctx); err != nil || again != 0 {
		t.Fatalf("a second sweep opened %d tasks (err %v)", again, err)
	}

	var tasks []gen.Task
	h.do("GET", "/projects/"+p.Id.String()+"/tasks", nil, &tasks)
	var curation gen.Task
	for _, task := range tasks {
		if task.Kind == "curation" {
			curation = task
		}
	}
	if curation.Id.String() == "" {
		t.Fatal("no curation task")
	}
	var detail gen.TaskDetail
	h.do("GET", "/tasks/"+curation.Id.String(), nil, &detail)
	if detail.Phase != "curation" {
		t.Fatalf("phase = %s", detail.Phase)
	}
	if !strings.Contains(deref(detail.Description), "Lesson 3") {
		t.Errorf("the brief should quote the entries whole: %q", deref(detail.Description))
	}

	// The curator answers, a person approves the answer, and the task finishes.
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO artifacts (task_id, phase, type, version, content, approved_at)
		 VALUES ($1, 'curation', 'report', 1, $2, now())`,
		curation.Id.String(), "Stale caches have bitten us six times; always invalidate on write."); err != nil {
		t.Fatal(err)
	}
	h.do("POST", "/tasks/"+curation.Id.String()+"/approve", nil, nil)
	if code := h.do("POST", "/tasks/"+curation.Id.String()+"/advance", nil, nil); code != 200 {
		t.Fatalf("finish the curation: %d", code)
	}

	var condensate gen.ProductEntry
	h.waitFor(t, "a condensed proposal", func() bool {
		for _, e := range h.productEntries(p.Id.String()) {
			if e.Kind == "lesson" && e.Status == "proposed" {
				condensate = e
				return true
			}
		}
		return false
	})
	if !strings.Contains(condensate.Content, "invalidate on write") {
		t.Fatalf("condensate = %+v", condensate)
	}

	// Until a person approves it, nothing has been folded away.
	if live := h.count(`SELECT count(*) FROM product_context WHERE project_id = $1 AND archived_at IS NOT NULL`, p.Id.String()); live != 0 {
		t.Fatalf("%d entries archived before anyone approved the condensate", live)
	}

	if code := h.do("POST", "/product/"+condensate.Id.String(), gen.ProductStatusChange{Status: "approved"}, nil); code != 200 {
		t.Fatalf("approve the condensate: %d", code)
	}
	if archived := h.count(`SELECT count(*) FROM product_context WHERE project_id = $1 AND archived_at IS NOT NULL`, p.Id.String()); archived != 6 {
		t.Fatalf("approving should archive the six originals, got %d", archived)
	}
	// Archived, not deleted: every original is still there to be read.
	if kept := h.count(`SELECT count(*) FROM product_context WHERE project_id = $1 AND title LIKE 'Lesson %'`, p.Id.String()); kept != 6 {
		t.Fatalf("the originals must survive, got %d", kept)
	}
	entries := h.productEntries(p.Id.String())
	if len(entries) < 7 {
		t.Fatalf("the screen should still show the originals: %d", len(entries))
	}
}

// A condensate must not feed the next condensation: that way the context is rewritten for ever
// and the words drift further from what anybody actually decided.
func TestACondensateDoesNotTriggerAnotherCondensation(t *testing.T) {
	h := newHarness(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	curator := product.NewCurator(h.pool, h.hub, slog.Default(), 50*time.Millisecond)
	condenser := product.NewCondenser(curator, h.svc, 2, 1<<20)

	p := h.oneProject("cl")
	h.seedLessons(p.Id.String(), 3)
	if opened, err := condenser.Sweep(ctx); err != nil || opened != 1 {
		t.Fatalf("first sweep opened %d (err %v)", opened, err)
	}

	// Pretend the curation finished and its condensate was approved: the originals are gone
	// from the live set, and the only entry left is the condensed one.
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO product_context (project_id, kind, title, content, version, status, created_by_kind, approved_at, condensed_from)
		 SELECT $1, 'lesson', 'Condensed lessons', 'Invalidate on write.', 1, 'approved', 'system', now(),
		        array_agg(id) FROM product_context WHERE project_id = $1 AND kind = 'lesson'`, p.Id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE product_context SET archived_at = now() WHERE project_id = $1 AND title LIKE 'Lesson %'`, p.Id.String()); err != nil {
		t.Fatal(err)
	}
	// Close the first curation task so it is not what stops the second sweep.
	if _, err := h.pool.Exec(ctx,
		`UPDATE tasks SET closed_at = now() WHERE project_id = $1 AND kind = 'curation'`, p.Id.String()); err != nil {
		t.Fatal(err)
	}

	if opened, err := condenser.Sweep(ctx); err != nil || opened != 0 {
		t.Fatalf("a condensate should not be condensed again: opened %d (err %v)", opened, err)
	}
}
