package e2e

import (
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// A plugin proposes to its own project's Brain (brain.propose): a proposal, never approved, with
// the plugin as its source, once per source however often the outside system says it again.
func TestAPluginProposesToTheBrainOncePerSource(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	adr := map[string]any{"brain": map[string]any{"kind": "decision", "title": "ADR 1: Postgres",
		"content": "We use Postgres because the workload is relational.", "source_id": "adr:docs/adr/0001-postgres.md@abc",
		"url": "https://github.com/acme/app/blob/main/docs/adr/0001-postgres.md"}}
	for _, delivery := range []string{"b1", "b2"} {
		if code, out := h.hook(p.slug, delivery, jsonBody(adr), nil); code != 200 {
			t.Fatalf("delivery %s: %d %v", delivery, code, out)
		}
	}
	var waiting []gen.BrainEntry
	h.do("GET", "/brain/proposals?project="+p.id, nil, &waiting)
	if len(waiting) != 1 {
		t.Fatalf("one proposal per source: %+v", waiting)
	}
	e := waiting[0]
	if e.Status != "proposed" || e.Kind != "decision" || e.Source.Kind != "plugin" || deref(e.Source.Name) != "echo" ||
		deref(e.Source.Id) != "adr:docs/adr/0001-postgres.md@abc" || e.CreatedByKind != "plugin" {
		t.Fatalf("a plugin's proposal: %+v", e)
	}
	bad := map[string]any{"brain": map[string]any{"kind": "preference", "title": "x", "content": "y", "source_id": "z"}}
	if code, _ := h.hook(p.slug, "b3", jsonBody(bad), nil); code == 200 {
		t.Fatal("a plugin proposes about the project, never about a person")
	}
}
