package process

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

var (
	// ErrInvalidEdit answers an edit that makes no sense on its own: an empty title, an
	// urgency off the scale, a kind no template describes.
	ErrInvalidEdit = errors.New("invalid edit")
	// ErrKindLocked answers a change of kind once work has started. The kind picks the
	// process; a task part-way through one cannot be dropped into another without its gates,
	// jobs and artifacts losing the phases they belong to.
	ErrKindLocked = errors.New("the kind of a task cannot change once work on it has started")
)

// EditParams names what to change; nil leaves a field as it is.
type EditParams struct {
	Title       *string
	Description *string
	Urgency     *int16
	Kind        *string
}

// Edit changes what a person said about a task. Title, description and urgency may change
// for as long as the task is open: they are words and a priority, and the next job reads
// them as they are then. Kind may change only before anything happened on the task, and moves it to the
// first phase of its new process. A closed task is history and is not edited; reopen it first.
// Every change is written to the task's trail with what it was before.
func (s *Service) Edit(ctx context.Context, taskID pgtype.UUID, p EditParams, actor Actor) (db.Task, error) {
	var out db.Task
	err := s.tx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, taskID)
		if err != nil {
			return err
		}
		if task.ClosedAt.Valid {
			return ErrTaskClosed
		}
		next := db.UpdateTaskFieldsParams{ID: taskID, Title: task.Title, Description: task.Description,
			Urgency: task.Urgency, Kind: task.Kind, Phase: task.Phase}
		changed := map[string]any{}
		if p.Title != nil {
			title := strings.TrimSpace(*p.Title)
			if title == "" {
				return fmt.Errorf("%w: a task needs a title", ErrInvalidEdit)
			}
			if title != task.Title {
				changed["title"] = map[string]any{"from": task.Title, "to": title}
				next.Title = title
			}
		}
		if p.Description != nil && *p.Description != task.Description {
			// The words themselves can be long; the trail says that they changed, the task
			// says what they are now.
			changed["description"] = map[string]any{"from_length": len(task.Description), "to_length": len(*p.Description)}
			next.Description = *p.Description
		}
		if p.Urgency != nil {
			if *p.Urgency < 1 || *p.Urgency > 4 {
				return fmt.Errorf("%w: urgency runs from 1 to 4", ErrInvalidEdit)
			}
			if *p.Urgency != task.Urgency {
				changed["urgency"] = map[string]any{"from": task.Urgency, "to": *p.Urgency}
				next.Urgency = *p.Urgency
			}
		}
		if p.Kind != nil && *p.Kind != task.Kind {
			first, err := s.retarget(ctx, q, task, *p.Kind)
			if err != nil {
				return err
			}
			changed["kind"] = map[string]any{"from": task.Kind, "to": *p.Kind}
			next.Kind, next.Phase = *p.Kind, first
		}
		if len(changed) == 0 {
			out = task
			return nil
		}
		out, err = q.UpdateTaskFields(ctx, next)
		if err != nil {
			return err
		}
		if next.Phase != task.Phase {
			if err := q.UpsertGate(ctx, db.UpsertGateParams{TaskID: taskID, Phase: next.Phase}); err != nil {
				return err
			}
		}
		if err := s.record(ctx, q, taskID, &task.Phase, next.Phase, "edit", actor, "", changed); err != nil {
			return err
		}
		fields := make([]string, 0, len(changed))
		for k := range changed {
			fields = append(fields, k)
		}
		return s.emit(ctx, q, "task.updated", taskID, map[string]any{"phase": next.Phase, "fields": fields})
	})
	return out, err
}

// retarget checks a change of kind and answers the phase the task starts over in.
func (s *Service) retarget(ctx context.Context, q *db.Queries, task db.Task, kind string) (string, error) {
	// A campaign is a different thing, not another process: it owns targets the other kinds
	// do not have, so turning one into or out of it would leave them dangling.
	if kind == CampaignKind || task.Kind == CampaignKind {
		return "", fmt.Errorf("%w: a campaign stays a campaign, and nothing else becomes one", ErrInvalidEdit)
	}
	worked, err := q.TaskWorkedOn(ctx, task.ID)
	if err != nil {
		return "", err
	}
	if worked {
		return "", ErrKindLocked
	}
	m, err := s.machine(ctx, task.ProjectID, kind)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidEdit, err)
	}
	return m.First().Name, nil
}

// Reopen brings closed work back, in the phase it was closed in, with a reason in the trail.
// The phase is entered afresh: its earlier approval no longer counts, since whoever reopens
// it is saying the work is not done, and the inbox and the autopilot see a new entry.
// Reopening an open task changes nothing.
func (s *Service) Reopen(ctx context.Context, taskID pgtype.UUID, actor Actor, reason string) (db.Task, error) {
	if strings.TrimSpace(reason) == "" {
		return db.Task{}, fmt.Errorf("%w: reopening a task requires a reason", ErrInvalidEdit)
	}
	var out db.Task
	err := s.tx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, taskID)
		if err != nil {
			return err
		}
		if !task.ClosedAt.Valid {
			out = task
			return nil
		}
		out, err = q.ReopenTask(ctx, taskID)
		if err != nil {
			return err
		}
		if err := q.UpsertGate(ctx, db.UpsertGateParams{TaskID: taskID, Phase: task.Phase}); err != nil {
			return err
		}
		if err := q.ClearGateApproval(ctx, db.ClearGateApprovalParams{TaskID: taskID, Phase: task.Phase}); err != nil {
			return err
		}
		if err := s.record(ctx, q, taskID, &task.Phase, task.Phase, "reopen", actor, reason, nil); err != nil {
			return err
		}
		return s.emit(ctx, q, "task.reopened", taskID, map[string]any{"phase": task.Phase, "reason": reason})
	})
	if err == nil {
		s.campaignFollows(ctx, taskID)
	}
	return out, err
}
