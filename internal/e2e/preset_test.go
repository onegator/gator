package e2e

import (
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// A preset is a snapshot. The project it came from goes on changing, and the projects started
// from it go their own way; neither may quietly rewrite the other.
func TestAProjectFromAPresetOwnsWhatItStartedWith(t *testing.T) {
	h := newHarness(t)
	var origin gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: uniqueSlug("origin"), Name: "The first product"}, &origin); code != 201 {
		t.Fatalf("project: %d", code)
	}
	originID := origin.Id.String()

	config := map[string]any{"config": map[string]any{
		"autopilot": map[string]any{"enabled": false},
		"policy":    map[string]any{"daily_budget_usd": 7}}}
	if code := h.do("PUT", "/projects/"+originID+"/config", config, nil); code != 200 {
		t.Fatalf("config: %d", code)
	}
	if code := h.do("POST", "/projects/"+originID+"/product",
		gen.NewProductEntry{Kind: gen.NewProductEntryKindVision, Title: "Why we build this",
			Content: "One person decides; agents do the work."}, nil); code != 201 {
		t.Fatalf("vision: %d", code)
	}
	var before []gen.ProductEntry
	h.do("GET", "/projects/"+originID+"/product", nil, &before)

	var preset gen.ProjectPreset
	if code := h.do("POST", "/presets", gen.NewProjectPreset{Name: uniqueSlug("preset"),
		Description: ptr("How we start a product here"), FromProjectId: origin.Id}, &preset); code != 201 {
		t.Fatalf("save preset: %d", code)
	}
	if len(preset.Product) == 0 {
		t.Fatal("the preset should carry the product context a person approved")
	}
	if preset.HasProcess == nil || !*preset.HasProcess {
		t.Error("the preset should say it carries a process, so a person knows what they get")
	}

	var made gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: uniqueSlug("made"), Name: "The next product",
		Preset: &preset.Name}, &made); code != 201 {
		t.Fatalf("project from preset: %d", code)
	}
	madeID := made.Id.String()

	var cfg gen.ProjectConfig
	if code := h.do("GET", "/projects/"+madeID+"/config", nil, &cfg); code != 200 {
		t.Fatalf("config of the new project: %d", code)
	}
	if _, ok := cfg.Config["policy"]; !ok {
		t.Errorf("the process should have been copied: %+v", cfg.Config)
	}

	var copied []gen.ProductEntry
	h.do("GET", "/projects/"+madeID+"/product", nil, &copied)
	if len(copied) == 0 {
		t.Fatal("the new project should start with the preset's product context")
	}
	// Copied, not shared: these are this project's own rows, with their own ids.
	for _, e := range copied {
		for _, original := range before {
			if e.Id == original.Id {
				t.Fatalf("the new project shares an entry with the project the preset came from: %s", e.Title)
			}
		}
	}

	// A later change to the origin must not reach a project that started from the preset.
	if code := h.do("POST", "/projects/"+originID+"/product",
		gen.NewProductEntry{Kind: gen.NewProductEntryKindVision, Title: "Why we build this",
			Content: "Changed our mind."}, nil); code != 201 {
		t.Fatalf("second vision: %d", code)
	}
	var after []gen.ProductEntry
	h.do("GET", "/projects/"+madeID+"/product", nil, &after)
	for _, e := range after {
		if strings.Contains(e.Content, "Changed our mind") {
			t.Fatal("a later change to the origin reached a project started from the preset")
		}
	}
}

// A name nobody saved is a mistake worth saying out loud, not an empty project and no hint.
func TestAnUnknownPresetIsReported(t *testing.T) {
	h := newHarness(t)
	name := "no-such-preset"
	if code := h.do("POST", "/projects", gen.NewProject{Slug: uniqueSlug("ghost"), Name: "Ghost", Preset: &name}, nil); code != 400 {
		t.Fatalf("an unknown preset should be refused, got %d", code)
	}
}

// Saving under a name that exists replaces it, so a workspace's starting points stay a short
// list rather than a pile of near-duplicates.
func TestSavingAPresetTwiceReplacesIt(t *testing.T) {
	h := newHarness(t)
	var p gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: uniqueSlug("twice"), Name: "Origin"}, &p)
	name := uniqueSlug("preset")

	var first, second gen.ProjectPreset
	h.do("POST", "/presets", gen.NewProjectPreset{Name: name, Description: ptr("first"), FromProjectId: p.Id}, &first)
	h.do("POST", "/presets", gen.NewProjectPreset{Name: name, Description: ptr("second"), FromProjectId: p.Id}, &second)
	if first.Id != second.Id {
		t.Errorf("saving the same name should replace, not add: %s then %s", first.Id, second.Id)
	}
	if second.Description != "second" {
		t.Errorf("description = %q, want the newer one", second.Description)
	}

	var list []gen.ProjectPreset
	h.do("GET", "/presets", nil, &list)
	seen := 0
	for _, item := range list {
		if item.Name == name {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the name appears %d times", seen)
	}
	if code := h.do("DELETE", "/presets/"+second.Id.String(), nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
}
