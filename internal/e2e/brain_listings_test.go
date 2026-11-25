package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/api/gen"
)

// A listing is not cut short by what the rules turn away: a search finds the reader's entry
// behind many they may not read, the review finds it behind many of another project's, and
// brain://me holds the person's own entry however much others share.
func TestListingsReadPastWhatTheRulesTurnAway(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, other := h.oneProject("listing"), h.oneProject("crowd")
	member := h.member(p.Id.String(), "member")
	word := fmt.Sprintf("grpc%d", time.Now().UnixNano())

	var own gen.BrainEntry
	h.do("POST", "/brain", brainDraft("project", "fact", "Transport "+word, "We speak "+word+".", &p), &own)
	if _, err := h.pool.Exec(ctx, `UPDATE brain_entries SET approved_at = now() - interval '200 days', updated_at = now() - interval '1 day' WHERE id = $1`, own.Id.String()); err != nil {
		t.Fatal(err)
	}
	// Proposals from private conversations of the project (no reader but their own), and another
	// project's forgotten facts.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO brain_entries (scope, project_id, kind, title, content, status, source_private)
		  SELECT 'project', $1::uuid, 'fact', 'Hidden ' || g || ' ' || $2::text, 'We speak ' || $2::text || '.', 'proposed', true
		  FROM generate_series(1, 100) g`, []any{p.Id.String(), word}},
		{`INSERT INTO brain_entries (scope, project_id, kind, title, content, status, approved_at)
		  SELECT 'project', $1::uuid, 'fact', 'Old ' || g, 'x', 'approved', now() - interval '300 days' FROM generate_series(1, 150) g`,
			[]any{other.Id.String()}},
		{`INSERT INTO brain_entries (scope, user_id, kind, title, content, status, visibility, approved_at)
		  SELECT 'user', u.id, 'expertise', 'Shared ' || g, 'x', 'approved', 'workspace', now() FROM generate_series(1, 120) g,
		         (SELECT id FROM users ORDER BY created_at LIMIT 1) u`, nil},
	} {
		if _, err := h.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if hits := h.brainSearch(member, "q="+word); len(hits) != 1 || hits[0].Id != own.Id {
		t.Fatalf("the search reads past a hundred hidden proposals: %+v", hits)
	}
	var review []gen.BrainEntry
	h.asJob(member, "GET", "/brain/review", nil, &review)
	found := false
	for _, e := range review {
		found = found || e.Id == own.Id
	}
	if !found {
		t.Fatalf("the review reads past another project's entries: %d entries", len(review))
	}

	var mine gen.BrainEntry
	h.asJob(member, "POST", "/brain", gen.NewBrainEntry{Scope: "user", Kind: "preference", Title: "Mine " + word, Content: "quiet"}, &mine)
	res, err := h.mcpSession(member).ReadResource(ctx, &sdk.ReadResourceParams{URI: "brain://me"})
	if err != nil || !strings.Contains(res.Contents[0].Text, "Mine "+word) || strings.Contains(res.Contents[0].Text, "Shared 1") {
		t.Fatalf("brain://me is the person's own: %v %+v", err, res)
	}
}

// A job's identity stops reaching the MCP server when the job ends.
func TestAFinishedJobsTokenReachesNoMCP(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	job := h.job(task, uniqueBackend(), "x", 0)
	token := h.identity(job.Id.String(), task.ProjectId.String())
	h.mcpSession(token) // works while the job is queued
	if code := h.do("POST", "/jobs/"+job.Id.String()+"/stop", gen.Reason{Reason: new("enough")}, nil); code != 200 && code != 202 {
		t.Fatalf("stop: %d", code)
	}
	h.waitJob(job.Id.String(), "stopped", "failed", "done")
	req, _ := http.NewRequest("POST", h.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 && resp.StatusCode != 403 {
		t.Fatalf("a finished job's token is refused: %d", resp.StatusCode)
	}
}
