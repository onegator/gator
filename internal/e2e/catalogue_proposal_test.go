package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/product"
)

// finishWithPaths records a finished job that changed these files, the way a runner's receipt
// does, and emits the event the curator listens for.
func (h *harness) finishWithPaths(projectID, taskID string, paths []string) {
	h.t.Helper()
	receipt, _ := json.Marshal(proto.Finish{Status: proto.StatusDone, ChangedPaths: paths, ChangedFiles: len(paths)})
	var jobID string
	if err := h.pool.QueryRow(context.Background(),
		`INSERT INTO jobs (task_id, project_id, phase, role, backend, status, receipt, finished_at)
		 VALUES ($1, $2, 'implementation', 'worker', 'fake', 'done', $3, now()) RETURNING id`,
		taskID, projectID, receipt).Scan(&jobID); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO events (type, aggregate, aggregate_id, payload) VALUES ('job.finished', 'job', $1, '{}'::jsonb)`,
		jobID); err != nil {
		h.t.Fatal(err)
	}
}

// A job working in a corner of the repository nobody has named should suggest one component —
// not one per file, and not until the corner has been worked in more than once.
func TestTheCuratorProposesOneComponentForADirectoryWorkedInTwice(t *testing.T) {
	h := newHarness(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go product.NewCurator(h.pool, h.hub, slog.Default(), 50*time.Millisecond).Run(ctx)
	time.Sleep(150 * time.Millisecond)

	task := h.task() // a bug in a fresh project
	project := task.ProjectId.String()
	paths := []string{
		"internal/billing/invoice.go",
		"internal/billing/invoice_test.go",
		"internal/billing/tax.go",
		"README.md", // at the root: not a part of a product
	}

	h.finishWithPaths(project, task.Id.String(), paths)
	// One run is a visit, not a home: nothing is proposed yet.
	time.Sleep(300 * time.Millisecond)
	if proposals := h.proposedComponents(project); len(proposals) != 0 {
		t.Fatalf("one job should not name a component: %+v", proposals)
	}

	h.finishWithPaths(project, task.Id.String(), paths)
	var proposals []gen.Component
	h.waitFor(t, "a proposed component", func() bool {
		proposals = h.proposedComponents(project)
		return len(proposals) > 0
	})
	if len(proposals) != 1 {
		t.Fatalf("three files in one directory should be one proposal, got %d: %+v", len(proposals), proposals)
	}
	if proposals[0].Key != "billing" || deref(proposals[0].Path) != "internal/billing" {
		t.Fatalf("proposal = %+v", proposals[0])
	}
	if deref(proposals[0].ProposedReason) == "" {
		t.Error("a proposal should say why it was made")
	}

	// A third run must not propose it again.
	h.finishWithPaths(project, task.Id.String(), paths)
	time.Sleep(300 * time.Millisecond)
	if again := h.proposedComponents(project); len(again) != 1 {
		t.Fatalf("the same directory was proposed twice: %+v", again)
	}

	// A proposal reaches no prompt until a person accepts it.
	if code := h.do("PUT", "/tasks/"+task.Id.String()+"/component",
		gen.TaskComponent{Key: ptr("billing")}, nil); code == 200 {
		before := h.jobContext(task.Id.String(), "worker")
		for _, doc := range before {
			if doc == "component|billing" {
				t.Fatal("a proposed component must not reach a prompt")
			}
		}
	}
	if code := h.do("POST", "/projects/"+project+"/components/billing/approve", nil, nil); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	if proposals := h.proposedComponents(project); len(proposals) != 0 {
		t.Fatalf("an approved component is no longer a proposal: %+v", proposals)
	}
}

func (h *harness) proposedComponents(projectID string) []gen.Component {
	h.t.Helper()
	var out []gen.Component
	h.do("GET", "/projects/"+projectID+"/components", nil, &out)
	var proposed []gen.Component
	for _, c := range out {
		if c.Status != nil && *c.Status == "proposed" {
			proposed = append(proposed, c)
		}
	}
	return proposed
}
