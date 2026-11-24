package conversations

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// taskCards keeps each task's own card up to date: it is posted with the task (opening the
// task's thread under it) and follows the task's title and phase. A hidden kind (a conversation
// with an agent) has no card: it is the conversation itself.
type taskCards struct {
	tasks taskReader
	kinds hiddenKinds // nil: nothing is hidden
}

// hiddenKinds answers which kinds of task are hidden in a project. process.Service is one.
type hiddenKinds interface {
	HiddenKind(ctx context.Context, projectID pgtype.UUID, kind string) (bool, error)
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
	if s.kinds != nil {
		if hidden, err := s.kinds.HiddenKind(ctx, task.ProjectID, task.Kind); err != nil || hidden {
			return nil, err
		}
	}
	return []PlacedCard{{Task: task, Root: true, Card: TaskCard(task)}}, nil
}
