package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
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

// holdingExec answers like an agent that takes its time: it says something, then waits for a
// correction or for the end, and keeps every correction it gets.
type holdingExec struct {
	steered chan string
}

func (e *holdingExec) Run(ctx context.Context, job proto.Job, io client.JobIO) proto.Finish {
	io.Emit("text", map[string]string{"text": "Looking at the diff."})
	for {
		select {
		case m := <-io.Steer:
			// What it says goes out before it says it heard: a test that stops it right after
			// must find these words in the answer.
			io.Emit("text", map[string]string{"text": "Narrowing it down."})
			e.steered <- m
		case <-ctx.Done():
			return proto.Finish{Status: proto.StatusStopped, StopReason: context.Cause(ctx).Error(),
				Usage: proto.Usage{Backend: job.Backend, DurationMS: 10}}
		}
	}
}

// A person's reply in the thread while the agent answers reaches the running agent as a
// correction; Stop under the answer stops it, and the answer ends with what it had said.
func TestSteerFromTheThreadAndStopFromTheMessage(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	project := task.ProjectId.String()
	if _, err := h.pool.Exec(context.Background(), "UPDATE projects SET process_config = $2 WHERE id = $1",
		project, `{"autopilot":{"backend":"`+backend+`"}}`); err != nil {
		t.Fatal(err)
	}
	exec := &holdingExec{steered: make(chan string, 4)}
	h.startRunner(backend, exec)
	general := h.generalOf(project)
	ask := h.say(project, general.Id.String(), "@reviewer look at PR #12", "reviewer")
	turn := h.turnOf(ask, project, "reviewer")
	h.waitJob(*turn.Job, "running")
	h.settledMessage(*turn.Reply, func(m gen.Message) bool { return m.Body == "Looking at the diff." })

	h.say(project, turn.Conversation, "only the checkout part, please")
	select {
	case m := <-exec.steered:
		if !strings.Contains(m, "only the checkout part, please") {
			t.Fatalf("steer: %q", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the correction never reached the agent")
	}
	// The words are in the answer before it is stopped: the stop must keep them, and a stop that
	// raced their delivery would not be the stop under test.
	h.settledMessage(*turn.Reply, func(m gen.Message) bool { return strings.Contains(m.Body, "Narrowing it down.") })

	var stopped gen.AgentTurn
	if code := h.do("POST", "/turns/"+turn.ID+"/stop", nil, &stopped); code != 202 {
		t.Fatalf("stop: %d", code)
	}
	reply := h.settledMessage(*turn.Reply, func(m gen.Message) bool { return m.Status == "final" })
	if !strings.HasPrefix(reply.Body, "Looking at the diff.\n\nNarrowing it down.") || !strings.Contains(reply.Body, "_@reviewer stopped (by ") ||
		reply.Turn == nil || reply.Turn.Status != "stopped" {
		t.Fatalf("the stopped answer: %q %+v", reply.Body, reply.Turn)
	}
	var e gen.Error
	if code := h.do("POST", "/turns/"+turn.ID+"/stop", nil, &e); code != 409 {
		t.Fatalf("stopping an ended turn: %d %+v", code, e)
	}
	var got gen.AgentTurn
	if code := h.do("GET", "/turns/"+turn.ID, nil, &got); code != 200 || got.Status != "stopped" || got.Hop != 0 || got.Tier != "chat" {
		t.Fatalf("turn: %d %+v", code, got)
	}
}

// message reads one message as the harness's person.
func (h *harness) message(id string) gen.Message {
	h.t.Helper()
	var m gen.Message
	if code := h.do("GET", "/messages/"+id, nil, &m); code != 200 {
		h.t.Fatalf("message %s: %d", id, code)
	}
	return m
}

// settledMessage waits for a message to be what ok wants.
func (h *harness) settledMessage(id string, ok func(gen.Message) bool) gen.Message {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		m := h.message(id)
		if ok(m) {
			return m
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("message %s stayed %s: %q (turn %+v)", id, m.Status, m.Body, m.Turn)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// An agent asked in chat about a task reads the branch the task's work was pushed to.
func TestAChatReplyReadsTheTasksBranch(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	project := task.ProjectId.String()
	if _, err := h.pool.Exec(context.Background(), "UPDATE projects SET process_config = $2 WHERE id = $1",
		project, `{"autopilot":{"backend":"`+backend+`"}}`); err != nil {
		t.Fatal(err)
	}
	def, primary := "main", true
	if code := h.do("PUT", "/projects/"+project+"/repos", gen.ProjectRepo{Name: "app", Url: "https://example.invalid/app.git", DefaultBranch: &def, Primary: &primary}, nil); code != 200 {
		t.Fatalf("repo: %d", code)
	}
	if _, err := h.pool.Exec(context.Background(), `INSERT INTO jobs (task_id, project_id, phase, role, backend, status, receipt, finished_at)
		VALUES ($1, $2, 'implementation', 'worker', 'claude', 'done', '{"branch":"gator/abc-123"}', now())`, task.Id.String(), project); err != nil {
		t.Fatal(err)
	}
	var thread gen.ConversationDetail
	h.do("GET", "/tasks/"+task.Id.String()+"/conversation", nil, &thread)
	h.turnOf(h.say(project, thread.Id.String(), "@reviewer is the fix right?", "reviewer"), project, "reviewer")
	raw, _ := h.dialRaw(h.runner, backend, proto.Version, 1)
	jobs := raw.lease(1)
	if len(jobs) != 1 || jobs[0].Repo == nil || jobs[0].Repo.ReadBranch != "gator/abc-123" {
		t.Fatalf("lease: %+v", jobs)
	}
}
