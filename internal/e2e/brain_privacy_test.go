package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/server/api/gen"
)

// handedExec keeps every job running until the test lets go, so its identity stays good, and
// remembers what each one was handed.
type handedExec struct {
	mu      sync.Mutex
	handed  map[string]proto.Job
	release chan struct{}
}

func (e *handedExec) Run(ctx context.Context, job proto.Job, _ client.JobIO) proto.Finish {
	e.mu.Lock()
	e.handed[job.JobID] = job
	e.mu.Unlock()
	select {
	case <-e.release:
	case <-ctx.Done():
	}
	return proto.Finish{Status: proto.StatusDone, Summary: "done", Usage: proto.Usage{Backend: job.Backend, DurationMS: 1, InputTokens: 1}}
}

// text is everything a job's prompt was handed, as one string.
func (e *handedExec) text(t *testing.T, jobID string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.mu.Lock()
		job, ok := e.handed[jobID]
		e.mu.Unlock()
		if ok {
			var b strings.Builder
			for _, d := range job.Context {
				b.WriteString(d.Title + "\n" + d.Body + "\n")
			}
			if job.Turn != nil {
				for _, d := range job.Turn.Conversation {
					b.WriteString(d.Title + "\n" + d.Body + "\n")
				}
			}
			return b.String()
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never ran", jobID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) userIDOf(token string) pgtype.UUID {
	h.t.Helper()
	var me gen.Me
	h.asJob(token, "GET", "/auth/me", nil, &me)
	var id pgtype.UUID
	_ = id.Scan(me.UserId.String())
	return id
}

// askAs mentions an agent in a conversation with token, and answers the turn's job.
func (h *harness) askAs(token, project, conversation, handle, body string) string {
	h.t.Helper()
	mentions := []gen.MemberRef{{Kind: gen.MemberRefKindAgent, Id: h.agentID(project, handle)}}
	var m gen.Message
	if code := h.asJob(token, "POST", "/conversations/"+conversation+"/messages", gen.NewMessage{Body: &body, Mentions: &mentions}, &m); code != 201 {
		h.t.Fatalf("ask: %d", code)
	}
	turn := h.turnOf(m, project, handle)
	if turn.Job == nil {
		h.t.Fatalf("no job for the turn: %+v", turn)
	}
	return *turn.Job
}

// A person's private context reaches the agent of a turn they asked for, and no other: not a
// colleague's turn, by any door — the package it is handed, the MCP server, the agent surface or
// the REST API — and not the record of what any job was handed. What they share with the project
// reaches everyone's. Their focus is worked out and as private. They can read it all and take it all away.
func TestAPersonsPrivateContextReachesOnlyTheirOwnTurns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.oneProject("priv")
	project := p.Id.String()
	backend := uniqueBackend()
	cfg, _ := json.Marshal(map[string]any{"policy": map[string]any{"default": map[string]any{"backend": backend}}})
	if _, err := h.pool.Exec(ctx, `UPDATE projects SET process_config = $2 WHERE id = $1`, project, string(cfg)); err != nil {
		t.Fatal(err)
	}
	exec := &handedExec{handed: map[string]proto.Job{}, release: make(chan struct{})}
	t.Cleanup(func() { close(exec.release) })
	h.startRunner(backend, exec)
	ada, bob := h.member(project, "member"), h.member(project, "member")
	word := fmt.Sprintf("tea%d", time.Now().UnixNano())

	var private, shared gen.BrainEntry
	h.asJob(ada, "POST", "/brain", gen.NewBrainEntry{Scope: "user", Kind: "preference", Title: "Mornings " + word,
		Content: "No meetings before ten, " + word + "."}, &private)
	vis := gen.BrainVisibility("project")
	h.asJob(ada, "POST", "/brain", gen.NewBrainEntry{Scope: "user", Kind: "expertise", Title: "Postgres " + word,
		Content: "Ada knows the planner, " + word + ".", Visibility: &vis, ProjectId: &p.Id}, &shared)
	if private.Visibility != "scope" || shared.Visibility != "project" {
		t.Fatalf("entries: %+v %+v", private, shared)
	}
	// Ada talks about a task this week: her focus is worked out from it.
	task := h.taskIn(project)
	var thread gen.ConversationDetail
	h.do("GET", "/tasks/"+task.Id.String()+"/conversation", nil, &thread)
	line := "I am on this one."
	h.asJob(ada, "POST", "/conversations/"+thread.Id.String()+"/messages", gen.NewMessage{Body: &line}, nil)
	if err := h.brain.Focus.Refresh(ctx, h.userIDOf(ada)); err != nil {
		t.Fatal(err)
	}
	var mine gen.MyBrain
	h.asJob(ada, "GET", "/me/brain", nil, &mine)
	var focus *gen.BrainEntry
	for i, e := range mine.Entries {
		if e.Kind == "focus" {
			focus = &mine.Entries[i]
		}
	}
	if len(mine.Entries) != 3 || focus == nil || focus.Confidence != "inferred" || focus.Status != "approved" || !strings.Contains(focus.Content, task.Title) {
		t.Fatalf("Ada's own Brain is her preference, her expertise and her focus: %+v", mine.Entries)
	}
	if mine.AgentsSee == "" {
		t.Fatal("and it says what agents see of it")
	}

	general := h.generalOf(project).Id.String()
	adasJob := h.askAs(ada, project, general, "reviewer", "@reviewer what do I prefer?")
	bobsJob := h.askAs(bob, project, general, "reviewer", "@reviewer what does Ada prefer?")

	// The package.
	adas, bobs := exec.text(t, adasJob), exec.text(t, bobsJob)
	for _, want := range []string{"No meetings before ten", "[brain:" + private.Id.String() + "]", "[brain:" + focus.Id.String() + "]", "[brain:" + shared.Id.String() + "]"} {
		if !strings.Contains(adas, want) {
			t.Errorf("Ada's turn is lent her context (%s):\n%s", want, adas)
		}
	}
	for _, never := range []string{"No meetings before ten", "Mornings " + word, private.Id.String(), focus.Id.String(), task.Title} {
		if strings.Contains(bobs, never) {
			t.Errorf("Bob's turn must never carry Ada's private context (%s):\n%s", never, bobs)
		}
	}
	if !strings.Contains(bobs, "[brain:"+shared.Id.String()+"]") {
		t.Errorf("what Ada shares with the project reaches Bob's turn too:\n%s", bobs)
	}
	// Nor the record of what either job was handed, which the project reads.
	for _, job := range []string{adasJob, bobsJob} {
		var pack gen.ContextPackage
		h.do("GET", "/jobs/"+job+"/context", nil, &pack)
		b, _ := json.Marshal(pack)
		if strings.Contains(string(b), "No meetings before ten") || strings.Contains(string(b), "Mornings "+word) {
			t.Errorf("the record of job %s keeps Ada's private words: %s", job, b)
		}
	}

	// The MCP server, with each job's own identity.
	search := func(token string) map[string]bool {
		var hits mcpHits
		cs := h.mcpSession(token)
		if ok, why := callTool(t, cs, "brain_search", map[string]any{"query": word}, &hits); !ok {
			t.Fatalf("brain_search: %s", why)
		}
		out := map[string]bool{}
		for _, hit := range hits.Hits {
			out[hit.ID] = true
		}
		return out
	}
	adaToken, bobToken := h.identity(adasJob, project), h.identity(bobsJob, project)
	if got := search(adaToken); !got[private.Id.String()] || !got[shared.Id.String()] {
		t.Errorf("over MCP Ada's turn finds her preference: %v", got)
	}
	if got := search(bobToken); got[private.Id.String()] || !got[shared.Id.String()] {
		t.Errorf("over MCP Bob's turn finds what she shares, never her preference: %v", got)
	}
	if ok, _ := callTool(t, h.mcpSession(bobToken), "brain_get", map[string]any{"id": private.Id.String()}, nil); ok {
		t.Error("nor reads it by id")
	}
	// The agent surface.
	var docs []gen.AgentDocument
	h.asJob(bobToken, "GET", "/agent/knowledge?q="+word, nil, &docs)
	for _, d := range docs {
		if strings.Contains(d.Body, "No meetings") {
			t.Errorf("gator-cli knowledge search from Bob's turn: %+v", d)
		}
	}
	// The REST API, for people.
	if got := hitIDs(h.brainSearch(bob, "q="+word)); got[private.Id.String()] || !got[shared.Id.String()] {
		t.Errorf("GET /brain for Bob: %v", got)
	}
	if got := hitIDs(h.brainSearch(h.admin, "q="+word)); got[private.Id.String()] {
		t.Errorf("GET /brain for a workspace admin: %v", got)
	}
	var bobs2 gen.MyBrain
	h.asJob(bob, "GET", "/me/brain", nil, &bobs2)
	if len(bobs2.Entries) != 0 {
		t.Errorf("Bob's own Brain holds nothing of Ada's: %+v", bobs2.Entries)
	}

	// Ada takes it all away.
	var gone gen.Forgotten
	if code := h.asJob(ada, "DELETE", "/me/brain", nil, &gone); code != 200 || gone.Deleted != 3 {
		t.Fatalf("forget: %d %+v", code, gone)
	}
	if got := search(adaToken); got[private.Id.String()] {
		t.Error("what was forgotten is not found any more")
	}
}
