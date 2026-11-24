package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/runners"
)

// identity mints the token a lease would hand the job.
func (h *harness) identity(jobID, projectID string) string {
	h.t.Helper()
	var job, project pgtype.UUID
	_ = job.Scan(jobID)
	_ = project.Scan(projectID)
	tok, err := auth.Tokens{Pool: h.pool}.IssueForJob(context.Background(), job, project, time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

// What a job's own identity may call is its kind's, checked by the server: a phase job works
// on its phase; a reply, chat or work, talks, looks things up and proposes, and never blocks a
// gate or writes a phase's documents; a summary may call nothing.
func TestAJobsIdentityIsScopedByItsKind(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	project := task.ProjectId.String()

	phase := h.job(task, uniqueBackend(), "work", 0) // allows agents on the task too
	general := h.generalOf(project)
	chat := h.turnOf(h.say(project, general.Id.String(), "@reviewer look", "reviewer"), project, "reviewer")
	var thread gen.ConversationDetail
	h.do("GET", "/tasks/"+task.Id.String()+"/conversation", nil, &thread)
	work := h.turnOf(h.say(project, thread.Id.String(), "@worker fix it", "worker"), project, "worker")
	if chat.Tier != "chat" || work.Tier != "work" {
		t.Fatalf("tiers: %s %s", chat.Tier, work.Tier)
	}
	var tid pgtype.UUID
	_ = tid.Scan(task.Id.String())
	summary, err := h.mgr.CreateJob(context.Background(), tid, runners.NewJob{Kind: runners.KindSummary, Tier: runners.TierChat,
		Role: "summarizer", Instruction: "x", CreatedBy: process.Actor{Kind: process.ActorSystem}})
	if err != nil {
		t.Fatal(err)
	}

	type call struct {
		cmd, method, path string
		body              any
	}
	calls := []call{
		{"task show", "GET", "/agent/task", nil},
		{"task blocked", "POST", "/agent/blocked", gen.AgentReason{Reason: "stuck"}},
		{"ask", "POST", "/agent/question", gen.AgentReason{Reason: "which one?"}},
		{"artifact put", "POST", "/agent/artifacts", gen.AgentArtifact{Type: "note", Content: "n"}},
		{"product propose", "POST", "/agent/product", gen.AgentProduct{Kind: "lesson", Title: "t", Content: "c"}},
		{"catalogue show", "GET", "/agent/catalogue", nil},
		{"knowledge search", "GET", "/agent/knowledge?q=x", nil},
		{"say", "POST", "/agent/say", gen.AgentSay{Body: "hello"}},
		{"thread show", "GET", "/agent/thread", nil},
		{"task create", "POST", "/agent/tasks", gen.AgentTaskProposal{Kind: "bug", Title: "t"}},
	}
	reply := map[string]bool{"say": true, "thread show": true, "task create": true, "knowledge search": true, "catalogue show": true, "product propose": true}
	jobs := []struct {
		name    string
		id      string
		allowed func(cmd string) bool
	}{
		{"chat reply", *chat.Job, func(c string) bool { return reply[c] }},
		{"work reply", *work.Job, func(c string) bool { return reply[c] }},
		{"summary", uuidOf(summary.ID), func(string) bool { return false }},
		{"phase", phase.Id.String(), func(string) bool { return true }},
	}
	for _, j := range jobs {
		token := h.identity(j.id, project)
		for _, c := range calls {
			code := h.asJob(token, c.method, c.path, c.body, nil)
			if want := j.allowed(c.cmd); want != (code == 200) || (!want && code != 403) {
				t.Errorf("%s job, %s: %d (allowed %v)", j.name, c.cmd, code, want)
			}
		}
	}
	var after gen.TaskDetail
	h.do("GET", "/tasks/"+task.Id.String(), nil, &after)
	if after.BlockedReason == nil {
		t.Fatal("the phase job's block should have landed")
	}
}
