package e2e

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

// trueVersions counts the versions of a lineage that hold now.
func (h *harness) trueVersions(lineage string) int {
	h.t.Helper()
	return h.count(`SELECT count(*) FROM brain_entries WHERE lineage = $1 AND status = 'approved' AND valid_until IS NULL`, lineage)
}

// The versions of an entry stay in order: approving a newer one makes the older ones (and older
// proposals) stop holding, and an older one cannot be approved over a newer one; an entry archived
// and approved again holds again, from when it was first approved.
func TestAnEntrysVersionsStayInOrder(t *testing.T) {
	h := newHarness(t)
	p := h.oneProject("versions")
	member := h.member(p.Id.String(), "member")
	var v1 gen.BrainEntry
	h.do("POST", "/brain", brainDraft("project", "runbook", "Deploy", "make deploy", &p), &v1)
	text := func(s string) *string { return &s }
	var v2, v3 gen.BrainEntry
	h.asJob(member, "PATCH", "/brain/"+v1.Id.String(), gen.BrainPatch{Content: text("make deploy ENV=prod"), Propose: new(true)}, &v2)
	h.do("PATCH", "/brain/"+v1.Id.String(), gen.BrainPatch{Content: text("make release")}, &v3)
	if v2.Version != 2 || v2.Status != "proposed" || v3.Version != 3 || v3.Status != "approved" {
		t.Fatalf("versions: %+v %+v", v2, v3)
	}
	var d gen.BrainDetail
	h.do("GET", "/brain/"+v2.Id.String(), nil, &d)
	if d.Entry.Status != "superseded" {
		t.Fatalf("an older proposal is stale once a newer version holds: %s", d.Entry.Status)
	}
	if code := h.do("POST", "/brain/"+v2.Id.String()+"/approve", nil, nil); code != 400 {
		t.Fatalf("an older version is not approved over a newer one: %d", code)
	}
	if n := h.trueVersions(v1.Lineage.String()); n != 1 {
		t.Fatalf("one version holds: %d", n)
	}

	// Archived, then approved again.
	h.do("POST", "/brain/"+v3.Id.String()+"/archive", nil, nil)
	var back gen.BrainEntry
	if code := h.do("POST", "/brain/"+v3.Id.String()+"/approve", nil, &back); code != 200 {
		t.Fatalf("approve again: %d", code)
	}
	if back.ValidUntil != nil || back.ApprovedAt == nil || !back.ApprovedAt.Equal(*v3.ApprovedAt) {
		t.Fatalf("it holds again, approved when it was first approved: %+v (was %v)", back, v3.ApprovedAt)
	}
	if !hitIDs(h.brainSearch(h.admin, "status=approved&project="+p.Id.String()+"&q=release"))[v3.Id.String()] {
		t.Fatal("and is found as approved")
	}
}

// Many people changing and approving one entry at once get a new version each or a conflict to
// retry, never a broken lineage or a server error.
func TestConcurrentChangesKeepOneLineage(t *testing.T) {
	h := newHarness(t)
	p := h.oneProject("race")
	member := h.member(p.Id.String(), "member")
	var first gen.BrainEntry
	h.do("POST", "/brain", brainDraft("project", "fact", "Region", "eu-west-1", &p), &first)
	run := func(n int, call func(i int) int) map[int]int {
		var mu sync.Mutex
		codes := map[int]int{}
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				code := call(i)
				mu.Lock()
				codes[code]++
				mu.Unlock()
			})
		}
		wg.Wait()
		return codes
	}
	codes := run(8, func(i int) int {
		content := fmt.Sprintf("eu-west-%d", i+2)
		return h.do("PATCH", "/brain/"+first.Id.String(), gen.BrainPatch{Content: &content}, nil)
	})
	if codes[200] == 0 || codes[200]+codes[409] != 8 {
		t.Fatalf("each edit is a version or a conflict: %v", codes)
	}
	if n := h.count(`SELECT count(*) - count(DISTINCT version) FROM brain_entries WHERE lineage = $1`, first.Lineage.String()); n != 0 {
		t.Fatal("no version number twice")
	}
	if n := h.trueVersions(first.Lineage.String()); n != 1 {
		t.Fatalf("one version holds after the edits: %d", n)
	}

	var proposals []string
	for i := range 4 {
		content := fmt.Sprintf("us-east-%d", i)
		var e gen.BrainEntry
		h.asJob(member, "PATCH", "/brain/"+first.Id.String(), gen.BrainPatch{Content: &content, Propose: new(true)}, &e)
		proposals = append(proposals, e.Id.String())
		time.Sleep(time.Millisecond)
	}
	codes = run(len(proposals), func(i int) int { return h.do("POST", "/brain/"+proposals[i]+"/approve", nil, nil) })
	if codes[500] != 0 || codes[200] == 0 {
		t.Fatalf("approvals at once: %v", codes)
	}
	if n := h.trueVersions(first.Lineage.String()); n != 1 {
		t.Fatalf("one version holds after the approvals: %d", n)
	}
}
