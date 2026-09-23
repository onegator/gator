package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// What a job was handed is worth reading afterwards: the size alone says how much, never what.
func TestAJobRecordsThePackageItWasHanded(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task() // a bug in planning
	// Something for the package to carry, beyond the task itself.
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO artifacts (task_id, phase, type, version, content) VALUES ($1, 'planning', 'plan', 1, $2)`,
		task.Id.String(), "# Plan\n\nFix the cache, not the caller."); err != nil {
		t.Fatal(err)
	}
	exec := &tokenExec{seen: make(chan struct{}, 1)}
	_, stop := h.startRunner(backend, exec)
	defer stop()
	job := h.job(task, backend, "work", 0)
	h.waitJob(job.Id.String(), "done", "running")

	var pack gen.ContextPackage
	if code := h.do("GET", "/jobs/"+job.Id.String()+"/context", nil, &pack); code != 200 {
		t.Fatalf("context: %d", code)
	}
	if len(pack.Documents) == 0 {
		t.Fatal("a job carrying a plan should record carrying it")
	}
	var plan *gen.ContextDocument
	for i, d := range pack.Documents {
		if strings.HasPrefix(d.Title, "plan from planning") {
			plan = &pack.Documents[i]
		}
	}
	if plan == nil {
		t.Fatalf("the plan should be in the package: %+v", pack.Documents)
	}
	if deref(plan.Origin) != "artifact" || !strings.Contains(deref(plan.Body), "Fix the cache") {
		t.Fatalf("plan = %+v", plan)
	}

	// The sum a person reads must be the number the receipt was measured by.
	full := h.waitJob(job.Id.String(), "done")
	if full.ContextBytes == nil || *full.ContextBytes != pack.Bytes {
		t.Fatalf("package says %d bytes, the job recorded %v", pack.Bytes, full.ContextBytes)
	}
}

// A document that did not fit is shown as left out, not quietly absent. Absence and exclusion
// look identical otherwise, and only one of them is a problem to fix.
func TestADocumentLeftOutSaysSo(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	// Four documents of 20 KiB each: the package holds 60 KiB, so the last cannot fit.
	big := strings.Repeat("x", 20000)
	for i, phase := range []string{"planning", "planning", "planning", "planning"} {
		if _, err := h.pool.Exec(context.Background(),
			`INSERT INTO artifacts (task_id, phase, type, version, content) VALUES ($1, $2, $3, 1, $4)`,
			task.Id.String(), phase, "note"+string(rune('a'+i)), big); err != nil {
			t.Fatal(err)
		}
	}
	exec := &tokenExec{seen: make(chan struct{}, 1)}
	_, stop := h.startRunner(backend, exec)
	defer stop()
	job := h.job(task, backend, "work", 0)
	h.waitJob(job.Id.String(), "done", "running")

	var pack gen.ContextPackage
	h.do("GET", "/jobs/"+job.Id.String()+"/context", nil, &pack)
	if pack.Dropped == 0 {
		t.Fatalf("with 80 KiB of documents and 60 KiB of room, something must be left out: %+v", pack)
	}
	var left *gen.ContextDocument
	for i, d := range pack.Documents {
		if d.Dropped {
			left = &pack.Documents[i]
		}
	}
	if left == nil || deref(left.LeftOut) == "" {
		t.Fatalf("a dropped document should say why: %+v", left)
	}
	if left.FullBytes == 0 {
		t.Error("a dropped document should still say how big it was")
	}
	if deref(left.Body) != "" {
		t.Error("a dropped document carries no body: the agent never saw it")
	}
	if pack.Bytes > 61000 {
		t.Errorf("the package counts only what reached the prompt, got %d", pack.Bytes)
	}
}

// The forecast: what a job started now would carry, before anything is spent. And it never
// reaches past its own project.
func TestThePreviewShowsWhatWouldBeSentAndNothingElse(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO product_context (project_id, kind, title, content, version, status, created_by_kind, approved_at)
		 VALUES ($1, 'decision', 'Ours', 'We cache for a minute.', 1, 'approved', 'user', now())`,
		task.ProjectId.String()); err != nil {
		t.Fatal(err)
	}
	other := h.oneProject("ctx")
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO product_context (project_id, kind, title, content, version, status, created_by_kind, approved_at)
		 VALUES ($1, 'decision', 'Theirs', 'We are acquiring Acme.', 1, 'approved', 'user', now())`,
		other.Id.String()); err != nil {
		t.Fatal(err)
	}

	var pack gen.ContextPackage
	if code := h.do("GET", "/tasks/"+task.Id.String()+"/context?role=planner", nil, &pack); code != 200 {
		t.Fatalf("preview: %d", code)
	}
	joined := ""
	for _, d := range pack.Documents {
		joined += d.Title + " " + deref(d.Body) + "\n"
	}
	if !strings.Contains(joined, "We cache for a minute") {
		t.Fatalf("the preview should carry this project's decisions:\n%s", joined)
	}
	if strings.Contains(joined, "Acme") {
		t.Fatal("the preview must never reach into another project")
	}
	if pack.Bytes == 0 {
		t.Error("the preview should say how much would be sent")
	}
}
