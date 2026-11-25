package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/runners"
)

// bearer adds a token to every request, as a client configured with
// --header "Authorization: Bearer …" does.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// mcpSession connects to /mcp over Streamable HTTP with token, as Claude Code would.
func (h *harness) mcpSession(token string) *sdk.ClientSession {
	h.t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: h.srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	h.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *sdk.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tool := range res.Tools {
		out = append(out, tool.Name)
	}
	sort.Strings(out)
	return out
}

// callTool calls a tool and decodes its structured result into out; it answers whether the call
// succeeded and, when it did not, why.
func callTool(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any, out any) (bool, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return false, err.Error()
	}
	if res.IsError {
		var why []string
		for _, c := range res.Content {
			if text, ok := c.(*sdk.TextContent); ok {
				why = append(why, text.Text)
			}
		}
		return false, strings.Join(why, " ")
	}
	if out != nil {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	return true, ""
}

type mcpHits struct {
	Hits []struct {
		ID, Title, Excerpt, Scope, Kind, Status string
	} `json:"hits"`
}

// Somebody points their own Claude Code at the Brain with their token: brain_search answers the
// same hits as the app's GET /brain, they read their own context as a resource, and what they
// propose waits for approval.
func TestTheBrainOverMCPAnswersWhatTheAppDoes(t *testing.T) {
	h := newHarness(t)
	p := h.oneProject("mcp")
	member := h.member(p.Id.String(), "member")
	word := fmt.Sprintf("rotate%d", time.Now().UnixNano())
	for i, title := range []string{"Keys", "Sessions", "Tokens"} {
		in := brainDraft("project", "decision", title, fmt.Sprintf("We %s keys %d times a week.", word, i+1), &p)
		if code := h.do("POST", "/brain", in, nil); code != 201 {
			t.Fatalf("write: %d", code)
		}
	}
	h.do("POST", "/brain", brainDraft("project", "principle", "Calm", "Say less, and mean it.", &p), nil)
	other := h.oneProject("mcpother")
	h.do("POST", "/brain", brainDraft("project", "decision", "Elsewhere", "We "+word+" nothing.", &other), nil)
	var mine gen.BrainEntry
	h.asJob(member, "POST", "/brain", gen.NewBrainEntry{Scope: "user", Kind: "preference", Title: "Mornings", Content: "No meetings before ten."}, &mine)

	app := h.brainSearch(member, "q="+url.QueryEscape(word))
	cs := h.mcpSession(member)
	if names := toolNames(t, cs); !slices.Equal(names, []string{"brain_cite", "brain_get", "brain_propose", "brain_search", "catalogue_show"}) {
		t.Fatalf("a person's tools: %v", names)
	}
	var hits mcpHits
	if ok, why := callTool(t, cs, "brain_search", map[string]any{"query": word}, &hits); !ok {
		t.Fatalf("brain_search: %s", why)
	}
	if len(app) != 3 || len(hits.Hits) != len(app) {
		t.Fatalf("the same hits: app %d, mcp %d", len(app), len(hits.Hits))
	}
	for i, a := range app {
		m := hits.Hits[i]
		if m.ID != a.Id.String() || m.Title != a.Title || m.Excerpt != a.Excerpt || m.Kind != string(a.Kind) || m.Status != string(a.Status) {
			t.Fatalf("hit %d differs: app %+v, mcp %+v", i, a, m)
		}
	}

	var got struct {
		Entry struct{ ID, Content string } `json:"entry"`
	}
	if ok, why := callTool(t, cs, "brain_get", map[string]any{"id": "[brain:" + app[0].Id.String() + "]"}, &got); !ok || got.Entry.ID != app[0].Id.String() {
		t.Fatalf("brain_get: %s %+v", why, got)
	}
	var proposed struct{ ID, Status string }
	if ok, why := callTool(t, cs, "brain_propose", map[string]any{"kind": "runbook", "title": "Rotate by hand", "content": "Run make rotate.", "project": p.Slug}, &proposed); !ok || proposed.Status != "proposed" {
		t.Fatalf("a person's MCP proposal waits for approval: %s %+v", why, proposed)
	}
	var cat struct {
		Components []struct{ Key string } `json:"components"`
	}
	if ok, why := callTool(t, cs, "catalogue_show", map[string]any{"project": other.Slug}, &cat); ok {
		t.Fatalf("a person reads no catalogue of a project they are not in: %s", why)
	}

	res, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "brain://me"})
	if err != nil || len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, "No meetings before ten") {
		t.Fatalf("brain://me: %v %+v", err, res)
	}
	res, err = cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "brain://project/" + p.Slug + "/principles"})
	if err != nil || !strings.Contains(res.Contents[0].Text, "Say less") {
		t.Fatalf("principles: %v %+v", err, res)
	}

	// Without a token there is no Brain.
	resp, err := http.Post(h.srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("anonymous: %v %d", err, resp.StatusCode)
	}
}

// A job's token reaches over MCP exactly what its kind reaches over /agent: a reply reads, proposes
// and speaks in its conversation; a summary gets nothing; a phase job gets its task too. Every call
// is written into the job's agent calls.
func TestMCPToolsFollowTheJobsScope(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	project := task.ProjectId.String()
	phase := h.job(task, uniqueBackend(), "work", 0)
	chat := h.turnOf(h.say(project, h.generalOf(project).Id.String(), "@reviewer look", "reviewer"), project, "reviewer")
	var tid pgtype.UUID
	_ = tid.Scan(task.Id.String())
	summary, err := h.mgr.CreateJob(context.Background(), tid, runners.NewJob{Kind: runners.KindSummary, Tier: runners.TierChat,
		Role: "summarizer", Instruction: "x", CreatedBy: process.Actor{Kind: process.ActorSystem}})
	if err != nil {
		t.Fatal(err)
	}

	reply := []string{"brain_cite", "brain_get", "brain_propose", "brain_search", "catalogue_show", "conversation_post", "thread_read"}
	all := []string{"ask", "brain_cite", "brain_get", "brain_propose", "brain_search", "catalogue_show", "conversation_post",
		"task_blocked", "task_show", "thread_read"}
	cases := []struct {
		name, job string
		tools     []string
	}{
		{"chat reply", *chat.Job, reply},
		{"summary", uuidOf(summary.ID), nil},
		{"phase", phase.Id.String(), all},
	}
	for _, c := range cases {
		cs := h.mcpSession(h.identity(c.job, project))
		if names := toolNames(t, cs); !slices.Equal(names, c.tools) {
			t.Errorf("%s job's tools: %v, want %v", c.name, names, c.tools)
		}
		if ok, _ := callTool(t, cs, "task_blocked", map[string]any{"reason": "stuck"}, nil); ok != slices.Contains(c.tools, "task_blocked") {
			t.Errorf("%s job blocking its task: %v", c.name, ok)
		}
	}

	cs := h.mcpSession(h.identity(*chat.Job, project))
	var thread struct {
		ConversationID string `json:"conversationId"`
	}
	if ok, why := callTool(t, cs, "thread_read", map[string]any{}, &thread); !ok || thread.ConversationID != chat.Conversation {
		t.Fatalf("a reply reads its own conversation: %s %+v", why, thread)
	}
	if ok, _ := callTool(t, cs, "thread_read", map[string]any{"conversation_id": uuidOf(tid)}, nil); ok {
		t.Fatal("and no other")
	}
	var said struct{ ID string }
	if ok, why := callTool(t, cs, "conversation_post", map[string]any{"body": "Looking at it now."}, &said); !ok {
		t.Fatalf("a reply says something in its conversation: %s", why)
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE id = $1 AND conversation_id = $2 AND author_kind = 'agent'`, said.ID, chat.Conversation); n != 1 {
		t.Fatal("as its agent, in its conversation")
	}
	callTool(t, cs, "brain_search", map[string]any{"query": "anything"}, nil)
	if n := h.count(`SELECT count(*) FROM agent_calls WHERE job_id = $1 AND command IN ('mcp thread_read', 'mcp conversation_post', 'mcp brain_search')`, *chat.Job); n < 3 {
		t.Fatalf("every call is in the job's agent calls: %d", n)
	}
}
