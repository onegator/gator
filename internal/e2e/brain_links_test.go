package e2e

import (
	"context"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// A link is not a way out of an entry's project or into somebody's private context: a writer
// links only to what belongs to the entry's project and they may read, never says "contradicts"
// themselves, and a forged contradiction supersedes nothing. A reader sees only the links whose
// targets they may read.
func TestLinksStayInsideTheirProjectAndTheReadersReach(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	task := h.task()
	p := gen.Project{Id: task.ProjectId}
	project := p.Id.String()
	other := h.oneProject("linked")
	ada := h.member(project, "member")
	// Two approved decisions in one project are what a condensation sweep looks for, in a database
	// every test shares: this project leaves none behind (it folds them away, as a condensate would).
	t.Cleanup(func() {
		if _, err := h.pool.Exec(context.Background(), `UPDATE brain_entries SET archived_at = now() WHERE project_id = $1`, project); err != nil {
			t.Error(err)
		}
	})

	var theirs, adas, ours gen.BrainEntry
	h.do("POST", "/brain", brainDraft("project", "decision", "Their database", "They use MySQL.", &other), &theirs)
	h.asJob(ada, "POST", "/brain", gen.NewBrainEntry{Scope: "user", Kind: "preference", Title: "Quiet mornings", Content: "No calls."}, &adas)
	h.do("POST", "/brain", brainDraft("project", "decision", "Our database", "We use Postgres.", &p), &ours)
	var otherTask gen.Task
	h.do("POST", "/projects/"+other.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "theirs"}, &otherTask)
	otherGeneral := h.generalOf(other.Id.String())

	link := func(kind, id, relation string) []gen.BrainLinkInput {
		rel := gen.BrainLinkInputRelation(relation)
		return []gen.BrainLinkInput{{TargetKind: gen.BrainLinkInputTargetKind(kind), TargetId: id, Relation: &rel}}
	}
	refused := []struct {
		name  string
		links []gen.BrainLinkInput
	}{
		{"a writer's own contradiction", link("entry", ours.Id.String(), "contradicts")},
		{"another project's decision", link("entry", theirs.Id.String(), "relates")},
		{"somebody's private context", link("entry", adas.Id.String(), "relates")},
		{"another project's task", link("task", otherTask.Id.String(), "relates")},
		{"another project's conversation", link("conversation", otherGeneral.Id.String(), "relates")},
		{"a component the project does not have", link("component", "nope", "relates")},
	}
	for _, c := range refused {
		in := brainDraft("project", "lesson", "Linked "+c.name, "x", &p)
		in.Links = &c.links
		if code := h.do("POST", "/brain", in, nil); code != 400 {
			t.Errorf("%s: %d", c.name, code)
		}
	}
	in := brainDraft("project", "lesson", "Linked properly", "x", &p)
	l := link("entry", ours.Id.String(), "relates")
	in.Links = &l
	if code := h.do("POST", "/brain", in, nil); code != 201 {
		t.Fatalf("a link inside the project: %d", code)
	}

	// An agent over MCP is held to the same.
	job := h.job(task, uniqueBackend(), "x", 0)
	cs := h.mcpSession(h.identity(job.Id.String(), project))
	if ok, _ := callTool(t, cs, "brain_propose", map[string]any{"kind": "decision", "title": "Supersede it", "content": "x",
		"links": []map[string]any{{"targetKind": "entry", "targetId": adas.Id.String(), "relation": "contradicts"}}}, nil); ok {
		t.Fatal("an agent cannot say what its proposal contradicts")
	}

	// A contradiction forged into the database supersedes nothing that is not a decision of the
	// entry's own project.
	var forged gen.BrainEntry
	prop := brainDraft("project", "decision", "Queues in Redis", "Jobs go through Redis.", &p)
	prop.Propose = new(true)
	h.do("POST", "/brain", prop, &forged)
	for _, target := range []string{theirs.Id.String(), adas.Id.String()} {
		if _, err := h.pool.Exec(ctx, `INSERT INTO brain_links (entry_id, target_kind, target_id, relation) VALUES ($1, 'entry', $2, 'contradicts')`,
			forged.Id.String(), target); err != nil {
			t.Fatal(err)
		}
	}
	if code := h.do("POST", "/brain/"+forged.Id.String()+"/approve", nil, nil); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	if n := h.count(`SELECT count(*) FROM brain_entries WHERE id IN ($1, $2) AND status = 'approved' AND valid_until IS NULL`, theirs.Id.String(), adas.Id.String()); n != 2 {
		t.Fatalf("both still hold: %d", n)
	}
	// And the reader is shown only the links they may follow.
	var d gen.BrainDetail
	h.do("GET", "/brain/"+forged.Id.String(), nil, &d)
	for _, l := range d.Links {
		if l.TargetId == adas.Id.String() {
			t.Fatal("a link to Ada's private context is not shown to anyone else")
		}
	}
	h.asJob(ada, "GET", "/brain/"+forged.Id.String(), nil, &d)
	seen := false
	for _, l := range d.Links {
		seen = seen || l.TargetId == adas.Id.String()
	}
	if !seen {
		t.Fatal("Ada sees the link to her own entry")
	}
}
