package process

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// AskKind is the hidden kind a conversation with an agent opens when it is about no task yet.
const AskKind = "ask"

var (
	// ErrHiddenKind answers a person creating a hidden kind, or changing a task into one: the
	// system opens those by itself.
	ErrHiddenKind = errors.New("this kind of task is opened by Gator itself")
	// ErrNotAnAsk answers turning a task that is not an ask into a task "for real".
	ErrNotAnAsk = errors.New("only a conversation with an agent becomes a task this way")
)

// HiddenKind reports whether a kind of task is hidden in the project: on no board and in no
// inbox. A kind without a template is not hidden.
func (s *Service) HiddenKind(ctx context.Context, projectID pgtype.UUID, kind string) (bool, error) {
	cat, err := s.templates.Catalog(ctx, projectID)
	if err != nil {
		return false, err
	}
	return cat[kind].Hidden, nil
}

// PromoteAskParams says what an ask becomes.
type PromoteAskParams struct {
	Kind string
	// Title and Description replace the ask's when not empty.
	Title, Description string
}

// PromoteAsk turns a conversation with an agent into a task "for real": its kind changes from
// ask to a kind of the process and it enters that kind's first phase. Unlike an edit of kind,
// this is allowed after work happened — the turns were the work, and their jobs, cost and
// working state stay with the task. Its conversation stays its conversation.
func (s *Service) PromoteAsk(ctx context.Context, taskID pgtype.UUID, p PromoteAskParams, actor Actor) (db.Task, error) {
	var out db.Task
	err := s.tx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, taskID)
		if err != nil {
			return err
		}
		if task.Kind != AskKind {
			return ErrNotAnAsk
		}
		if task.ClosedAt.Valid {
			return ErrTaskClosed
		}
		if p.Kind == CampaignKind {
			return fmt.Errorf("%w: a conversation becomes a feature, a bug or another single task", ErrInvalidEdit)
		}
		m, err := s.machine(ctx, task.ProjectID, p.Kind)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidEdit, err)
		}
		if m.Hidden() {
			return ErrHiddenKind
		}
		next := db.UpdateTaskFieldsParams{ID: taskID, Title: task.Title, Description: task.Description,
			Urgency: task.Urgency, Kind: p.Kind, Phase: m.First().Name}
		if t := strings.TrimSpace(p.Title); t != "" {
			next.Title = t
		}
		if d := strings.TrimSpace(p.Description); d != "" {
			next.Description = d
		}
		if out, err = q.UpdateTaskFields(ctx, next); err != nil {
			return err
		}
		if err := q.UpsertGate(ctx, db.UpsertGateParams{TaskID: taskID, Phase: next.Phase}); err != nil {
			return err
		}
		changed := map[string]any{"kind": map[string]any{"from": task.Kind, "to": p.Kind}}
		if err := s.record(ctx, q, taskID, &task.Phase, next.Phase, "edit", actor, "made a task of a conversation with an agent", changed); err != nil {
			return err
		}
		return s.emit(ctx, q, "task.updated", taskID, map[string]any{"phase": next.Phase, "fields": []string{"kind"}, "from": AskKind})
	})
	return out, err
}
