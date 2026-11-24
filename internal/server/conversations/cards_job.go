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
}

// jobCards shows each job in its task's thread as one card that follows the job from queued to
// done: a task with many jobs gets one card per job, never one per event.
type jobCards struct {
	jobs jobReader
}

// Events implements CardSource.
func (jobCards) Events() []string {
	return []string{"job.queued", "job.leased", "job.started", "job.stalled", "job.resumed", "job.requeued",
		"job.no_runner", "job.provisioned", "job.provision_failed", "job.finished"}
}

// Cards implements CardSource.
func (s jobCards) Cards(ctx context.Context, e db.EventsAfterRow) ([]PlacedCard, error) {
	job, err := s.jobs.GetJob(ctx, e.AggregateID)
	if err != nil {
		return nil, ignoreGone(err)
	}
	task, ok, err := taskOf(ctx, s.jobs, job.TaskID)
	if !ok {
		return nil, err
	}
	return []PlacedCard{{Task: task, Card: JobCard(job)}}, nil
}

// JobCard is the card of one job: its role and phase, its status, and Stop, Steer and the logs
// while it runs.
func JobCard(j db.Job) Card {
	id := uuidString(j.ID)
	who := j.Role
	if who == "" {
		who = "agent"
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
	return Card{
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
		},
	}
}
