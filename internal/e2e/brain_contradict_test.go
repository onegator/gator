package e2e

import (
	"net/url"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

func linksOf(h *harness, id string) map[string]string {
	h.t.Helper()
	var d gen.BrainDetail
	if code := h.do("GET", "/brain/"+id, nil, &d); code != 200 {
		h.t.Fatalf("detail: %d", code)
	}
	out := map[string]string{}
	for _, l := range d.Links {
		out[l.TargetId] = string(l.Relation)
	}
	return out
}

// A proposed decision much like an approved one but saying something else is linked to it as
// contradicting it; approving it supersedes the old one from then on, and what held before is
// still there to be asked about. A decision about something else, or one saying the same, is not.
func TestAProposalThatContradictsADecisionSupersedesItOnceApproved(t *testing.T) {
	h := newHarness(t)
	p := h.oneProject("contra")
	auth := "auth"
	decision := func(title, content string, propose bool) gen.BrainEntry {
		in := gen.NewBrainEntry{Scope: "component", Kind: "decision", Title: title, Content: content, ProjectId: &p.Id, ComponentKey: &auth}
		if propose {
			in.Propose = new(true)
		}
		var e gen.BrainEntry
		if code := h.do("POST", "/brain", in, &e); code != 201 {
			t.Fatalf("write %q: %d", title, code)
		}
		return e
	}
	old := decision("Session tokens are JWT", "Sessions are signed JWTs with HS256.", false)
	time.Sleep(20 * time.Millisecond)
	before := time.Now()
	time.Sleep(20 * time.Millisecond)

	newer := decision("Session tokens are PASETO", "Sessions are PASETO v4 local tokens; no JWT anywhere.", true)
	unrelated := decision("Billing runs nightly", "Invoices are made at 02:00.", true)
	same := decision("Session tokens, again", "Sessions are signed JWTs with HS256.", true)
	if linksOf(h, newer.Id.String())[old.Id.String()] != "contradicts" {
		t.Fatalf("the PASETO proposal contradicts the JWT decision: %v", linksOf(h, newer.Id.String()))
	}
	if _, ok := linksOf(h, unrelated.Id.String())[old.Id.String()]; ok {
		t.Fatal("a decision about something else contradicts nothing")
	}
	if _, ok := linksOf(h, same.Id.String())[old.Id.String()]; ok {
		t.Fatal("one that says the same is no contradiction")
	}

	if code := h.do("POST", "/brain/"+newer.Id.String()+"/approve", nil, nil); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	var d gen.BrainDetail
	h.do("GET", "/brain/"+old.Id.String(), nil, &d)
	if d.Entry.Status != "superseded" || d.Entry.ValidUntil == nil {
		t.Fatalf("the JWT decision stopped being true: %+v", d.Entry)
	}
	now := hitIDs(h.brainSearch(h.admin, "status=approved&project="+p.Id.String()+"&q=session+or+sessions"))
	if now[old.Id.String()] || !now[newer.Id.String()] {
		t.Fatalf("what holds now: %v", now)
	}
	then := hitIDs(h.brainSearch(h.admin, "project="+p.Id.String()+"&q=session+or+sessions&at="+url.QueryEscape(before.UTC().Format(time.RFC3339Nano))))
	if !then[old.Id.String()] || then[newer.Id.String()] {
		t.Fatalf("what held before the change: %v", then)
	}
}
