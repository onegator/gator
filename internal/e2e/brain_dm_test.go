package e2e

import (
	"context"
	"strings"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/api/gen"
)

// dmProposal has an agent propose a decision from inside a DM of the workspace admin's, and
// answers the project, a member who is not in the DM, and the proposal.
func (h *harness) dmProposal(title string) (project string, outsider string, entry string) {
	h.t.Helper()
	task := h.task()
	project = task.ProjectId.String()
	if _, err := h.pool.Exec(context.Background(), "UPDATE projects SET process_config = $2 WHERE id = $1", project,
		`{"autopilot":{"backend":"`+uniqueBackend()+`"}}`); err != nil {
		h.t.Fatal(err)
	}
	outsider = h.member(project, "member")
	pid := task.ProjectId
	agents := []openapi_types.UUID{h.agentID(project, "reviewer")}
	var dm gen.ConversationDetail
	if code := h.do("POST", "/conversations/dm", gen.NewDirectMessage{Members: []openapi_types.UUID{}, ProjectId: &pid, Agents: &agents}, &dm); code != 201 {
		h.t.Fatalf("dm: %d", code)
	}
	turn := h.turnOf(h.say(project, dm.Id.String(), "we are buying Acme, keep it quiet"), project, "reviewer")
	var w gen.AgentWrite
	if code := h.asJob(h.identity(*turn.Job, project), "POST", "/agent/product",
		gen.AgentProduct{Kind: "decision", Title: title, Content: "Acme is ours on Friday."}, &w); code != 200 || w.Id == nil {
		h.t.Fatalf("propose from the DM: %d %+v", code, w)
	}
	return project, outsider, w.Id.String()
}

// What an agent proposes from a DM is the DM's people's until one of them approves it: no other
// member of the project finds it at any door — the product context, the inbox, /brain, MCP — nor
// decides on it; once approved, it is the project's.
func TestADMsProposalStaysWithTheDMAtEveryDoor(t *testing.T) {
	h := newHarness(t)
	project, outsider, id := h.dmProposal("Acquire Acme")

	listed := func(token string) bool {
		var out []gen.ProductEntry
		h.asJob(token, "GET", "/projects/"+project+"/product", nil, &out)
		for _, e := range out {
			if e.Id.String() == id {
				return true
			}
		}
		return false
	}
	inbox := func(token string) bool {
		var out []gen.Decision
		h.asJob(token, "GET", "/inbox?all=true&projectId="+project, nil, &out)
		for _, d := range out {
			if d.Id == "product:"+id || strings.Contains(d.Subject.Name, "Acme") {
				return true
			}
		}
		return false
	}
	if !listed(h.admin) || !inbox(h.admin) {
		t.Fatal("the DM's own people see the proposal")
	}
	if listed(outsider) || inbox(outsider) {
		t.Fatal("a member outside the DM must not see it in the product context or the inbox")
	}
	if code := h.asJob(outsider, "POST", "/product/"+id, gen.ProductStatusChange{Status: "approved"}, nil); code != 404 {
		t.Fatalf("nor approve it through the old door: %d", code)
	}
	if code := h.asJob(outsider, "GET", "/brain/"+id, nil, nil); code != 404 {
		t.Fatalf("nor read it in /brain: %d", code)
	}
	var waiting []gen.BrainEntry
	h.asJob(outsider, "GET", "/brain/proposals?project="+project, nil, &waiting)
	if len(waiting) != 0 {
		t.Fatalf("nor in the proposals: %+v", waiting)
	}
	var hits mcpHits
	callTool(t, h.mcpSession(outsider), "brain_search", map[string]any{"query": "Acme"}, &hits)
	if len(hits.Hits) != 0 {
		t.Fatalf("nor over MCP: %+v", hits)
	}
	var payload string
	if err := h.pool.QueryRow(context.Background(), `SELECT payload::text FROM events WHERE type = 'brain.proposed' AND payload->>'entry_id' = $1`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "conversation_id") || strings.Contains(payload, "task_id") {
		t.Fatalf("the project's event says nothing of where it was made: %s", payload)
	}

	var approved gen.ProductEntry
	if code := h.do("POST", "/product/"+id, gen.ProductStatusChange{Status: "approved"}, &approved); code != 200 || approved.Status != "approved" {
		t.Fatalf("someone in the DM approves it: %d %+v", code, approved)
	}
	if !listed(outsider) {
		t.Fatal("once approved it is the project's")
	}
}
