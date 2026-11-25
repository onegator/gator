package e2e

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

func (h *harness) brainSearch(token, query string) []gen.BrainHit {
	h.t.Helper()
	var out []gen.BrainHit
	if code := h.asJob(token, "GET", "/brain?"+query, nil, &out); code != 200 {
		h.t.Fatalf("search %q: %d", query, code)
	}
	return out
}

func hitIDs(hits []gen.BrainHit) map[string]bool {
	out := map[string]bool{}
	for _, h := range hits {
		out[h.Id.String()] = true
	}
	return out
}

func brainDraft(scope gen.BrainScope, kind gen.BrainKind, title, content string, project *gen.Project) gen.NewBrainEntry {
	in := gen.NewBrainEntry{Scope: scope, Kind: kind, Title: title, Content: content}
	if project != nil {
		in.ProjectId = &project.Id
	}
	return in
}

// An entry is written, found, changed as a new version and decided on through one set of rules:
// a project's members read and approve its entries, others do not even find them, the workspace
// is approved by its admins, and a change keeps the version it replaced.
func TestBrainEntriesVersionsAndVerdicts(t *testing.T) {
	h := newHarness(t)
	p := h.oneProject("brain")
	member, viewer, outsider := h.member(p.Id.String(), "member"), h.member(p.Id.String(), "viewer"), h.person()
	word := fmt.Sprintf("paseto%d", time.Now().UnixNano())

	var first gen.BrainEntry
	if code := h.do("POST", "/brain", brainDraft("project", "decision", "Tokens", "Auth uses "+word+", not JWT.", &p), &first); code != 201 {
		t.Fatalf("write: %d", code)
	}
	if first.Status != "approved" || first.Version != 1 || first.Source.Kind != "manual" {
		t.Fatalf("a member's entry is approved as written: %+v", first)
	}
	if !hitIDs(h.brainSearch(member, "q="+word))[first.Id.String()] || !hitIDs(h.brainSearch(viewer, "q="+word))[first.Id.String()] {
		t.Fatal("the project's people find it")
	}
	if hitIDs(h.brainSearch(outsider, "q="+word))[first.Id.String()] {
		t.Fatal("somebody outside the project must not find it")
	}
	if code := h.asJob(outsider, "GET", "/brain/"+first.Id.String(), nil, nil); code != 404 {
		t.Fatalf("nor read it: %d", code)
	}

	// A change is a new version; the old one stops being true and stays.
	content := "Auth uses " + word + " v4 local, not JWT, because keys rotate."
	var second gen.BrainEntry
	if code := h.asJob(member, "PATCH", "/brain/"+first.Id.String(), gen.BrainPatch{Content: &content}, &second); code != 200 {
		t.Fatalf("revise: %d", code)
	}
	if second.Version != 2 || second.Lineage != first.Lineage || second.Supersedes == nil || *second.Supersedes != first.Id || second.Status != "approved" {
		t.Fatalf("a new version: %+v", second)
	}
	var detail gen.BrainDetail
	if code := h.do("GET", "/brain/"+first.Id.String(), nil, &detail); code != 200 || len(detail.Versions) != 2 {
		t.Fatalf("history: %d %+v", code, detail.Versions)
	}
	if detail.Entry.Status != "superseded" || detail.Entry.ValidUntil == nil {
		t.Fatalf("the first version stopped being true: %+v", detail.Entry)
	}
	if hits := h.brainSearch(member, "status=approved&q="+word); len(hits) != 1 || hits[0].Id != second.Id {
		t.Fatalf("only the true version is approved: %+v", hits)
	}

	// A proposal waits; a viewer does not decide it, a member does.
	in := brainDraft("project", "lesson", "E2E needs the database", "make db-up before go test.", &p)
	in.Propose = new(true)
	var proposal gen.BrainEntry
	if code := h.asJob(member, "POST", "/brain", in, &proposal); code != 201 || proposal.Status != "proposed" {
		t.Fatalf("propose: %d %+v", code, proposal)
	}
	var waiting []gen.BrainEntry
	h.asJob(member, "GET", "/brain/proposals?project="+p.Id.String(), nil, &waiting)
	if len(waiting) != 1 || waiting[0].Id != proposal.Id {
		t.Fatalf("the proposal waits: %+v", waiting)
	}
	if code := h.asJob(viewer, "POST", "/brain/"+proposal.Id.String()+"/approve", nil, nil); code != 403 {
		t.Fatalf("a viewer does not approve: %d", code)
	}
	var approved gen.BrainEntry
	if code := h.asJob(member, "POST", "/brain/"+proposal.Id.String()+"/approve", nil, &approved); code != 200 || approved.Status != "approved" || approved.ApprovedAt == nil {
		t.Fatalf("approve: %d %+v", code, approved)
	}
	if code := h.asJob(member, "POST", "/brain/"+proposal.Id.String()+"/reject", nil, nil); code != 400 {
		t.Fatalf("only a proposal is rejected: %d", code)
	}

	// The workspace is its admins' to approve; anyone proposes to it.
	var convention gen.BrainEntry
	conv := brainDraft("workspace", "convention", "Errors "+word, "Wrap with %w.", nil)
	conv.Topic = new(gen.NewBrainEntryTopic("language"))
	if code := h.asJob(member, "POST", "/brain", conv, &convention); code != 201 || convention.Status != "proposed" {
		t.Fatalf("a member proposes to the workspace: %d %+v", code, convention)
	}
	if code := h.do("POST", "/brain/"+convention.Id.String()+"/approve", nil, nil); code != 200 {
		t.Fatalf("an admin approves it: %d", code)
	}
	if !hitIDs(h.brainSearch(outsider, "scope=workspace&q="+url.QueryEscape("Errors "+word)))[convention.Id.String()] {
		t.Fatal("the workspace is everyone's")
	}

	// A person's context is theirs.
	var pref gen.BrainEntry
	if code := h.asJob(member, "POST", "/brain", gen.NewBrainEntry{Scope: "user", Kind: "preference", Title: "Short PRs " + word, Content: "Under 300 lines."}, &pref); code != 201 || pref.Status != "approved" {
		t.Fatalf("my preference: %d %+v", code, pref)
	}
	if !hitIDs(h.brainSearch(member, "q=Short+PRs+"+word))[pref.Id.String()] {
		t.Fatal("I find my own")
	}
	if hitIDs(h.brainSearch(viewer, "q=Short+PRs+"+word))[pref.Id.String()] || hitIDs(h.brainSearch(h.admin, "q=Short+PRs+"+word))[pref.Id.String()] {
		t.Fatal("nobody else does, not even a workspace admin")
	}
	if code := h.do("POST", "/brain", gen.NewBrainEntry{Scope: "project", Kind: "preference", Title: "x", Content: "y", ProjectId: &p.Id}, nil); code != 400 {
		t.Fatalf("a preference is a person's: %d", code)
	}
}

// An approved entry nobody has cited for ninety days comes up for review; one a job cited lately
// does not.
func TestEntriesNobodyCitesComeUpForReview(t *testing.T) {
	h := newHarness(t)
	p := h.oneProject("review")
	var old, used gen.BrainEntry
	h.do("POST", "/brain", brainDraft("project", "runbook", "Restart the queue", "kill -HUP the worker", &p), &old)
	h.do("POST", "/brain", brainDraft("project", "runbook", "Rotate the keys", "make rotate", &p), &used)
	task := h.taskIn(p.Id.String())
	job := h.job(task, uniqueBackend(), "x", 0)
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `UPDATE brain_entries SET approved_at = now() - interval '100 days' WHERE id IN ($1, $2)`,
		old.Id.String(), used.Id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO brain_citations (job_id, entry_id, how) VALUES ($1, $2, 'cited')`,
		job.Id.String(), used.Id.String()); err != nil {
		t.Fatal(err)
	}
	var review []gen.BrainEntry
	if code := h.do("GET", "/brain/review?project="+p.Id.String(), nil, &review); code != 200 {
		t.Fatalf("review: %d", code)
	}
	if len(review) != 1 || review[0].Id != old.Id {
		t.Fatalf("only the entry nobody cites: %+v", review)
	}
}
