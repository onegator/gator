package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// The vertical slice of the Brain: a decision agreed in a task's thread comes back, once the thread
// has gone quiet, as a proposal and a card in the thread; a person approves it; a job in another
// task about the same component is handed it and cites it. Both agents are recorded Claude Code
// sessions, replayed.
func TestADecisionAgreedInAThreadReachesTheNextTaskOfItsComponent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, _ := h.repoProject()
	project := p.Id.String()
	cfg, _ := json.Marshal(map[string]any{"policy": map[string]any{"default": map[string]any{"backend": "claude"}}})
	if _, err := h.pool.Exec(ctx, `UPDATE projects SET process_config = $2 WHERE id = $1`, project, string(cfg)); err != nil {
		t.Fatal(err)
	}
	h.putComponent(project, gen.ComponentInput{Key: "auth", Name: "Auth", Kind: gen.ComponentInputKindApi})
	about := func(title string) gen.Task {
		var task gen.Task
		if code := h.do("POST", "/projects/"+project+"/tasks", gen.NewTask{Kind: "bug", Title: title}, &task); code != 201 {
			t.Fatalf("task: %d", code)
		}
		key := "auth"
		if code := h.do("PUT", "/tasks/"+task.Id.String()+"/component", gen.TaskComponent{Key: &key}, nil); code != 200 {
			t.Fatalf("component: %d", code)
		}
		return task
	}
	first := about("Sessions expire early")

	// The people of the task talk it through in its thread and settle on something.
	var thread gen.ConversationDetail
	if code := h.do("GET", "/tasks/"+first.Id.String()+"/conversation", nil, &thread); code != 200 {
		t.Fatalf("thread: %d", code)
	}
	colleague := h.member(project, "member")
	said := map[string]int64{}
	ids := map[int64]string{}
	for i, line := range []string{
		"Should the session tokens stay JWT, or move to PASETO?",
		"PASETO v4 local: there is no algorithm field to get wrong.",
		"Agreed: PASETO v4 local, and we rotate the keys weekly.",
		"Good, I will write it down in the PR as well.",
	} {
		body := line
		token := h.admin
		if i%2 == 1 {
			token = colleague
		}
		var m gen.Message
		if code := h.asJob(token, "POST", "/conversations/"+thread.Id.String()+"/messages", gen.NewMessage{Body: &body}, &m); code != 201 {
			t.Fatalf("say: %d", code)
		}
		said[line], ids[m.Seq] = m.Seq, m.Id.String()
	}
	// Cards share the thread's numbering, so the number the recorded answer quotes is filled in.
	agreed := said["Agreed: PASETO v4 local, and we rotate the keys weekly."]

	// The thread goes quiet; the curator reads it, with the recorded answer.
	h.claudeRunner(project, "synthetic-curator-decision.ndjson")
	t.Setenv("FAKE_CLAUDE_SUBST", fmt.Sprintf("s/__SEQ__/%d/g", agreed))
	h.curator.Quiet = 0
	if n, err := h.curator.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("the quiet thread is curated once: %d %v", n, err)
	}
	if n, _ := h.curator.Sweep(ctx); n != 0 {
		t.Fatal("and not again while its curation is written")
	}
	var proposal gen.BrainEntry
	h.waitFor(t, "the curator's proposal", func() bool {
		var waiting []gen.BrainEntry
		h.do("GET", "/brain/proposals?project="+project, nil, &waiting)
		for _, e := range waiting {
			if e.Title == "Sessions use PASETO v4" {
				proposal = e
				return true
			}
		}
		return false
	})
	if proposal.Scope != "component" || deref(proposal.ComponentKey) != "auth" || proposal.Kind != "decision" {
		t.Fatalf("a decision about the task's component: %+v", proposal)
	}
	if proposal.Source.Kind != "message" || deref(proposal.Source.Id) != ids[agreed] || !strings.Contains(deref(proposal.Source.Excerpt), "rotate the keys weekly") {
		t.Fatalf("it says where it was settled, in whose words: %+v", proposal.Source)
	}
	if proposal.SourceTaskId == nil || *proposal.SourceTaskId != first.Id || proposal.SourceConversationId == nil || *proposal.SourceConversationId != thread.Id {
		t.Fatalf("and in which task and thread: %+v", proposal)
	}
	card := func() (gen.Message, bool) {
		var page gen.MessagePage
		h.do("GET", "/conversations/"+thread.Id.String()+"/messages?limit=100", nil, &page)
		for _, m := range page.Messages {
			if m.CardType != nil && *m.CardType == "brain_proposal" {
				return m, true
			}
		}
		return gen.Message{}, false
	}
	h.waitFor(t, "the Brain's card in the thread", func() bool { _, ok := card(); return ok })
	if m, _ := card(); !strings.Contains(m.Body, "1 proposal") {
		t.Fatalf("the card: %+v", m.Body)
	}
	if n, _ := h.curator.Sweep(ctx); n != 0 {
		t.Fatal("a thread with nothing new said is not read again")
	}

	// A person approves it.
	if code := h.do("POST", "/brain/"+proposal.Id.String()+"/approve", nil, nil); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	h.waitFor(t, "the card to say it is decided", func() bool {
		m, _ := card()
		b, _ := json.Marshal(m.Card)
		return strings.Contains(string(b), `"state":"resolved"`)
	})

	// Another task about the same component: its job is handed the decision, and cites it.
	second := about("Refresh tokens")
	t.Setenv("FAKE_CLAUDE_FIXTURE", fixturePath("synthetic-brain-cite.ndjson"))
	t.Setenv("FAKE_CLAUDE_SUBST", "s/__ENTRY__/"+proposal.Id.String()+"/g")
	job := h.job(second, "claude", "Plan the refresh endpoint.", 0)
	done := h.waitJob(job.Id.String(), "done", "failed")
	if done.Status != "done" {
		t.Fatalf("job: %s %s", done.Status, deref(done.StopReason))
	}
	var pack gen.ContextPackage
	h.do("GET", "/jobs/"+job.Id.String()+"/context", nil, &pack)
	handed := false
	for _, d := range pack.Documents {
		if strings.Contains(d.Title, "[brain:"+proposal.Id.String()+"]") && strings.Contains(deref(d.Body), "PASETO v4 local") {
			handed = true
		}
	}
	if !handed {
		t.Fatalf("the job was handed the decision: %+v", pack.Documents)
	}
	var detail gen.BrainDetail
	h.do("GET", "/brain/"+proposal.Id.String(), nil, &detail)
	hows := map[string]bool{}
	for _, c := range detail.Citations {
		if c.JobId == job.Id {
			hows[string(c.How)] = true
		}
	}
	if !hows["packed"] || !hows["cited"] {
		t.Fatalf("the decision says it was handed to that job and cited by it: %+v", detail.Citations)
	}
}

// fixturePath is a recording of the claude backend's.
func fixturePath(name string) string {
	wd, _ := os.Getwd()
	return filepath.Join(wd, "..", "runner", "backend", "claude", "testdata", name)
}
