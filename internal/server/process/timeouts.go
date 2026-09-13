package process

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// ExpirePhases blocks every open, unblocked task that has sat in its phase longer than the
// template timeout. A blocked task lands in the inbox; a human decides what happens next.
// Returns the ids of tasks blocked in this pass.
func (s *Service) ExpirePhases(ctx context.Context, now time.Time) ([]string, error) {
	tasks, err := db.New(s.pool).ListOpenUnblockedTasks(ctx)
	if err != nil {
		return nil, err
	}
	machines := map[string]*Machine{}
	var blocked []string
	for _, t := range tasks {
		key := uuidString(t.ProjectID) + "/" + t.Kind
		m, ok := machines[key]
		if !ok {
			m, err = s.machine(ctx, t.ProjectID, t.Kind)
			if err != nil {
				return blocked, err
			}
			machines[key] = m
		}
		p, err := m.Phase(t.Phase)
		if err != nil || p.Timeout == 0 || !t.PhaseEnteredAt.Valid {
			continue
		}
		waited := now.Sub(t.PhaseEnteredAt.Time)
		if waited < p.Timeout.Std() {
			continue
		}
		reason := fmt.Sprintf("phase %q exceeded its %s timeout (waited %s)", t.Phase, p.Timeout.Std(), waited.Round(time.Minute))
		err = s.tx(ctx, func(q *db.Queries) error {
			cur, err := q.GetTaskForUpdate(ctx, t.ID)
			if err != nil {
				return err
			}
			if cur.BlockedReason != nil || cur.ClosedAt.Valid || cur.Phase != t.Phase {
				return nil // changed under us; skip
			}
			if err := q.SetTaskBlocked(ctx, db.SetTaskBlockedParams{ID: t.ID, BlockedReason: &reason}); err != nil {
				return err
			}
			if err := q.SetGateBlocked(ctx, db.SetGateBlockedParams{TaskID: t.ID, Phase: t.Phase, BlockedReason: &reason, BlockedBy: ptr("automation")}); err != nil {
				return err
			}
			if err := s.record(ctx, q, t.ID, &t.Phase, t.Phase, "auto_block", Actor{Kind: ActorSystem}, reason, map[string]any{"timeout": p.Timeout.Std().String(), "waited": waited.String()}); err != nil {
				return err
			}
			blocked = append(blocked, uuidString(t.ID))
			return s.emit(ctx, q, "gate.blocked", t.ID, map[string]any{"phase": t.Phase, "reason": reason, "timeout": true})
		})
		if err != nil {
			return blocked, err
		}
	}
	return blocked, nil
}

// Unblock clears a block set by automation or a human so the task can move again.
func (s *Service) Unblock(ctx context.Context, taskID pgtype.UUID, actor Actor, reason string) error {
	if reason == "" {
		return errors.New("unblock requires a reason")
	}
	return s.tx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, taskID)
		if err != nil {
			return err
		}
		if task.BlockedReason == nil {
			return nil
		}
		if err := q.SetTaskBlocked(ctx, db.SetTaskBlockedParams{ID: taskID}); err != nil {
			return err
		}
		if err := q.ClearGateBlocked(ctx, db.ClearGateBlockedParams{TaskID: taskID, Phase: task.Phase}); err != nil {
			return err
		}
		if err := s.record(ctx, q, taskID, &task.Phase, task.Phase, "unblock", actor, reason, nil); err != nil {
			return err
		}
		return s.emit(ctx, q, "gate.unblocked", taskID, map[string]any{"phase": task.Phase, "reason": reason})
	})
}
