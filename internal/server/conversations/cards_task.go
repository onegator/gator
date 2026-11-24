package conversations

import (
	"context"

	"github.com/onegator/gator/internal/server/store/db"
)

// taskCards keeps each task's own card up to date: it is posted with the task (opening the
// task's thread under it) and follows the task's title and phase.
type taskCards struct {
	tasks taskReader
}

// Events implements CardSource.
func (taskCards) Events() []string {
	return []string{"task.created", "task.updated", "task.phase_changed", "task.closed", "task.reopened"}
}

// Cards implements CardSource.
func (s taskCards) Cards(ctx context.Context, e db.EventsAfterRow) ([]PlacedCard, error) {
	task, ok, err := taskOf(ctx, s.tasks, e.AggregateID)
	if !ok {
		return nil, err
	}
	return []PlacedCard{{Task: task, Root: true, Card: TaskCard(task)}}, nil
}
