package conversations

import (
	"fmt"

	"github.com/onegator/gator/internal/server/store/db"
)

// taskCardKey is what a task's own card is about.
func taskCardKey(t db.Task) string { return "task:" + uuidString(t.ID) }

// TaskCard is the card a task has where its thread hangs: its title, kind and phase, with what
// can be done from it. Replies under it are the task's thread.
func TaskCard(t db.Task) Card {
	id := uuidString(t.ID)
	state := t.Phase
	if t.ClosedAt.Valid {
		state = "closed"
	}
	return Card{
		Key:  taskCardKey(t),
		Type: CardTask,
		Body: fmt.Sprintf("**%s** · %s · %s", t.Title, t.Kind, state),
		Data: map[string]any{
			"ref":     Ref("task", id, map[string]any{"taskId": id}),
			"title":   t.Title,
			"state":   state,
			"summary": t.Kind,
			"actions": []string{"open", "assign"},
		},
	}
}
