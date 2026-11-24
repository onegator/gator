package conversations

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// jobReader is what the job cards read. *db.Queries is one.
type jobReader interface {
	taskReader
	GetJob(ctx context.Context, id pgtype.UUID) (db.Job, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	JobProgress(ctx context.Context, jobID pgtype.UUID) (db.JobProgressRow, error)
}

// jobCards shows each job in its task's thread as one card that follows the job from queued to
// done: a task with many jobs gets one card per job, never one per event.
type jobCards struct {
	jobs jobReader
}

// Events implements CardSource.
func (jobCards) Events() []string {
	return []string{"job.queued", "job.leased", "job.started", "job.stalled", "job.resumed", "job.requeued",
		"job.no_runner", "job.provisioned", "job.provision_failed", "job.finished", EventJobProgress}
}

// Cards implements CardSource.
func (s jobCards) Cards(ctx context.Context, e db.EventsAfterRow) ([]PlacedCard, error) {
	job, err := s.jobs.GetJob(ctx, e.AggregateID)
	if err != nil {
		return nil, ignoreGone(err)
	}
	if job.Kind == summaryKind {
		return nil, nil // Gator's own housekeeping on the conversation, not anybody's work
	}
	task, ok, err := taskOf(ctx, s.jobs, job.TaskID)
	if !ok {
		return nil, err
	}
	var agent *db.Agent
	if job.AgentID.Valid {
		a, err := s.jobs.GetAgent(ctx, job.AgentID)
		if err != nil && ignoreGone(err) != nil {
			return nil, err
		}
		if err == nil {
			agent = &a
		}
	}
	progress, err := s.jobs.JobProgress(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	card := JobCard(job, agent)
	withProgress(card, progress)
	return []PlacedCard{{Task: task, Card: card}}, nil
}

// EventJobProgress says an agent's job called more tools: its card follows. The agents package
// writes it, a few times a second at most, while it streams a reply.
const EventJobProgress = "job.progress"

// withProgress puts on a job's card how far its agent has got: the tools it called, the last
// one by name. The card is where a reply's tool calls go, not the answer.
func withProgress(c Card, p db.JobProgressRow) {
	if p.ToolCalls == 0 {
		return
	}
	c.Data["progress"] = map[string]any{"toolCalls": p.ToolCalls, "lastTool": p.LastTool}
	c.Data["summary"] = fmt.Sprintf("%s · %d tool calls, last %s", c.Data["summary"], p.ToolCalls, p.LastTool)
}

// JobCard is the card of one job: who does it (the agent, else its role) and in which phase,
// its status, and Stop, Steer and the logs while it runs.
func JobCard(j db.Job, agent *db.Agent) Card {
	id := uuidString(j.ID)
	who := j.Role
	if who == "" {
		who = "agent"
	}
	if agent != nil {
		who = "@" + agent.Handle
	}
	actions := []string{"logs"}
	switch j.Status {
	case "queued", "leased", "running", "stalled":
		actions = []string{"stop", "steer", "logs"}
	}
	state := j.Status
	if j.Status == "queued" && j.UnassignableSince.Valid {
		state = "no_runner" // queued, and no runner online can take it
	}
	card := Card{
		Key:  "job:" + id,
		Type: CardJob,
		Body: fmt.Sprintf("Job · %s in %s · %s", who, j.Phase, state),
		Data: map[string]any{
			"ref":     Ref("job", id, map[string]any{"taskId": uuidString(j.TaskID)}),
			"title":   who,
			"state":   state,
			"summary": j.Phase + " · " + j.Backend,
			"actions": actions,
			"phase":   j.Phase,
			"role":    j.Role,
		},
	}
	if agent != nil {
		card.Data["agent"] = agentRef(*agent)
	}
	return card
}

// agentRef is how a card names an agent: enough to draw it without asking.
func agentRef(a db.Agent) map[string]any {
	return map[string]any{"id": uuidString(a.ID), "handle": a.Handle, "displayName": a.DisplayName, "avatar": a.Avatar}
}
