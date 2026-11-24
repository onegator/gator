package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
)

// generalOf is a project's #general.
func (h *harness) generalOf(projectID string) gen.Conversation {
	h.t.Helper()
	var list []gen.Conversation
	h.do("GET", "/projects/"+projectID+"/conversations?kind=channel", nil, &list)
	for _, c := range list {
		if c.Name != nil && *c.Name == "general" {
			return c
		}
	}
	h.t.Fatal("no #general")
	return gen.Conversation{}
}

// agentID is the id of a project's agent.
func (h *harness) agentID(projectID, handle string) openapi_types.UUID {
	h.t.Helper()
	var id openapi_types.UUID
	if err := h.pool.QueryRow(context.Background(), "SELECT id FROM agents WHERE project_id = $1 AND handle = $2", projectID, handle).Scan(&id); err != nil {
		h.t.Fatalf("@%s: %v", handle, err)
	}
	return id
}

// say posts a message as the harness's person, mentioning these agents.
func (h *harness) say(projectID, conversation, body string, handles ...string) gen.Message {
	h.t.Helper()
	var mentions []gen.MemberRef
	for _, handle := range handles {
		mentions = append(mentions, gen.MemberRef{Kind: gen.MemberRefKindAgent, Id: h.agentID(projectID, handle)})
	}
	var msg gen.Message
	if code := h.do("POST", "/conversations/"+conversation+"/messages", gen.NewMessage{Body: &body, Mentions: &mentions}, &msg); code != 201 {
		h.t.Fatalf("post: %d", code)
	}
	return msg
}

type turnRow struct {
	ID, Conversation, Status, Tier string
	Job, Reply                     *string
	Refusal                        *string
}

// turnOf waits for the turn a message started for an agent, until it has a job or ended.
func (h *harness) turnOf(msg gen.Message, projectID, handle string) turnRow {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var r turnRow
		err := h.pool.QueryRow(context.Background(), `SELECT id::text, conversation_id::text, status, tier, job_id::text, reply_message_id::text, refusal
			FROM agent_turns WHERE trigger_message_id = $1 AND agent_id = $2`, msg.Id.String(), h.agentID(projectID, handle)).
			Scan(&r.ID, &r.Conversation, &r.Status, &r.Tier, &r.Job, &r.Reply, &r.Refusal)
		if err == nil && (r.Job != nil || r.Status != "queued") {
			return r
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("no turn of @%s for %q: %v", handle, msg.Body, err)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// A runner older than protocol v3 would run a reply as a phase — commit, push, object to itself
// — so it is handed only phase jobs. A v3 runner gets the reply with its kind, its read-only
// tier, its agent and the conversation it answers in.
func TestOnlyV3RunnersTakeReplies(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	project := task.ProjectId.String()
	if _, err := h.pool.Exec(context.Background(), "UPDATE projects SET process_config = $2 WHERE id = $1",
		project, `{"autopilot":{"backend":"`+backend+`"}}`); err != nil {
		t.Fatal(err)
	}
	general := h.generalOf(project)
	root := h.say(project, general.Id.String(), "PR #12 changes how checkout totals are rounded")
	var thread gen.ConversationDetail
	h.do("GET", "/messages/"+root.Id.String()+"/thread", nil, &thread)
	ask := h.say(project, thread.Id.String(), "@reviewer look at PR #12", "reviewer")
	turn := h.turnOf(ask, project, "reviewer")
	if turn.Job == nil || turn.Tier != "chat" {
		t.Fatalf("turn: %+v", turn)
	}

	old, reg := h.dialRaw(h.runner, backend, proto.Version-1, 1)
	var r proto.Registered
	if err := reg.Into(&r); err != nil || r.Version != proto.Version-1 {
		t.Fatalf("the server speaks N-1: %+v %v", r, err)
	}
	if jobs := old.lease(1); len(jobs) != 0 {
		t.Fatalf("a v2 runner was handed %+v", jobs)
	}
	old.c.CloseNow()

	other, err := runnerToken(h)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := h.dialRaw(other, backend, proto.Version, 1)
	jobs := cur.lease(1)
	if len(jobs) != 1 || jobs[0].JobID != *turn.Job {
		t.Fatalf("a v3 runner takes the reply: %+v", jobs)
	}
	j := jobs[0]
	if j.Kind != proto.KindReply || !j.ReadOnly() || j.Agent == nil || j.Agent.Handle != "reviewer" || j.Turn == nil || j.Turn.ID != turn.ID {
		t.Fatalf("lease: %+v", j)
	}
	if !strings.Contains(j.Turn.Ask, "asks you, @reviewer, in a thread in #general") || !strings.Contains(j.Turn.Repeat, "look at PR #12") ||
		!strings.HasPrefix(j.Guide, "# Role: reviewer") {
		t.Fatalf("turn: %+v guide %q", j.Turn, j.Guide)
	}
	var job gen.Job
	h.do("GET", "/jobs/"+j.JobID, nil, &job)
	if job.ContextBytes == nil || *job.ContextBytes < len(j.Turn.Ask) {
		t.Fatalf("the brief is measured: %+v", job.ContextBytes)
	}
	var pkg gen.ContextPackage
	h.do("GET", "/jobs/"+j.JobID+"/context", nil, &pkg)
	conversation := 0
	for _, d := range pkg.Documents {
		if deref(d.Origin) == "conversation" {
			conversation++
		}
	}
	if conversation == 0 {
		raw, _ := json.Marshal(pkg)
		t.Fatalf("the conversation is recorded among what the job was handed: %s", raw)
	}
}
