package e2e

import (
	"context"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/automations"
)

// agentHandle is the handle of the agent a job belongs to, or "".
func (h *harness) agentHandle(job gen.Job) string {
	h.t.Helper()
	if job.AgentId == nil {
		return ""
	}
	var handle string
	if err := h.pool.QueryRow(context.Background(), "SELECT handle FROM agents WHERE id = $1", job.AgentId.String()).Scan(&handle); err != nil {
		h.t.Fatal(err)
	}
	return handle
}

// The autopilot's jobs are the work of the agent playing their role: @researcher's discovery,
// @planner's plan. A project that made its own agent for a role has that agent do it, and the
// template does not change.
func TestAutopilotJobsAreSignedByTheRolesAgent(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	p, task := h.featureTask(backend)
	task = h.approveAdvance(task) // idea → discovery

	job, created := h.ensure(task)
	if !created || job.Role != "researcher" || h.agentHandle(job) != "researcher" || job.Kind != "phase" || job.Tier != "work" {
		t.Fatalf("discovery should be @researcher's phase job: %+v (@%s)", job, h.agentHandle(job))
	}

	// The project's own researcher takes over the role.
	if _, err := h.pool.Exec(context.Background(), `INSERT INTO agents (project_id, handle, display_name, role_key, instructions, source)
		VALUES ($1, 'deep-researcher', 'Deep researcher', 'researcher', 'Read every paper.', '{"kind":"custom"}')`, p.Id.String()); err != nil {
		t.Fatal(err)
	}
	_, other := h.featureTaskIn(p, backend)
	job, created = h.ensure(h.approveAdvance(other))
	if !created || h.agentHandle(job) != "deep-researcher" || job.Role != "researcher" {
		t.Fatalf("the project's own agent plays the role: %+v (@%s)", job, h.agentHandle(job))
	}
	exec := &recordingExec{}
	_, stop := h.startRunner(backend, exec)
	defer stop()
	h.waitJob(job.Id.String(), "done")
	if got := exec.last(t).Guide; got != "Read every paper." {
		t.Fatalf("an agent with its own instructions works from them, got %q", got)
	}
}

// An automation's work is its agent's: error triage has @triage look into the incident, while
// the incident's later phases stay with the agents of their roles.
func TestErrorTriageIsTheTriageAgentsWork(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	h.switchOn(p.id, automations.ErrorTriage, nil)
	h.incident(p.slug, "agent")
	triaged := h.tasksOf(p.id, automations.ErrorTriage)
	if len(triaged) != 1 {
		t.Fatalf("triaged: %d", len(triaged))
	}
	job, created := h.ensure(triaged[0])
	if !created || job.Role != "worker" || h.agentHandle(job) != "triage" {
		t.Fatalf("the incident's implementation is @triage's: %+v (@%s)", job, h.agentHandle(job))
	}
}

// featureTaskIn is featureTask in an existing project.
func (h *harness) featureTaskIn(p gen.Project, _ string) (gen.Project, gen.Task) {
	h.t.Helper()
	var task gen.Task
	if code := h.do("POST", "/projects/"+p.Id.String()+"/tasks", gen.NewTask{Kind: "feature", Title: "Another"}, &task); code != 201 {
		h.t.Fatalf("task: %d", code)
	}
	return p, h.allowAgents(task)
}
