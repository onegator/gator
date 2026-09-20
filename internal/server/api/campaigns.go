package api

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/process"
)

// GetCampaign answers how far a fleet change has got.
func (s *Server) GetCampaign(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleViewer); !ok {
		return
	}
	p, err := s.Process.CampaignProgressFor(r.Context(), fromUUID(taskId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCampaign(p))
}

// AddCampaignTargets gives a campaign the places it has to reach.
func (s *Server) AddCampaignTargets(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.CampaignTargets
	if !decode(w, r, &in) {
		return
	}
	instruction := deref(in.Instruction)
	targets := make([]process.Target, 0, len(in.Targets))
	for _, t := range in.Targets {
		target := process.Target{Key: t.Key, Title: deref(t.Title), Instruction: instruction}
		if t.Kind != nil {
			target.Kind = string(*t.Kind)
		}
		if t.ProjectId != nil {
			target.ProjectID = pgtype.UUID{Bytes: *t.ProjectId, Valid: true}
		}
		targets = append(targets, target)
	}
	p, err := s.Process.AddTargets(r.Context(), fromUUID(taskId), targets, ActorFromContext(r.Context()))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCampaign(p))
}

// SkipCampaignTarget drops one target, with a reason.
func (s *Server) SkipCampaignTarget(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.SkipTarget
	if !decode(w, r, &in) {
		return
	}
	p, err := s.Process.SkipTarget(r.Context(), fromUUID(taskId), in.Key, in.Reason, ActorFromContext(r.Context()))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCampaign(p))
}

func toCampaign(p process.CampaignProgress) gen.Campaign {
	out := gen.Campaign{Done: p.Done, Blocked: p.Blocked, Waiting: p.Waiting, Skipped: p.Skipped,
		Targets: make([]gen.CampaignTarget, 0, len(p.Targets))}
	for _, t := range p.Targets {
		target := gen.CampaignTarget{Key: t.Key, Skipped: t.Skipped,
			ProjectId: toUUIDPtr(t.ProjectID), TaskId: toUUIDPtr(t.TaskID)}
		if t.Title != "" {
			title := t.Title
			target.Title = &title
		}
		if t.Phase != "" {
			phase := t.Phase
			target.Phase = &phase
		}
		if t.Blocked != "" {
			blocked := t.Blocked
			target.Blocked = &blocked
		}
		if t.SkipReason != "" {
			reason := t.SkipReason
			target.SkipReason = &reason
		}
		closed := t.Closed
		target.Closed = &closed
		out.Targets = append(out.Targets, target)
	}
	return out
}
