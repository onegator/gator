package api

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// UpdateTask edits an open task. A member may; a viewer reads.
func (s *Server) UpdateTask(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.TaskPatch
	if !decode(w, r, &in) {
		return
	}
	p := process.EditParams{Title: in.Title, Description: in.Description}
	if in.Urgency != nil {
		u := int16(*in.Urgency)
		p.Urgency = &u
	}
	if in.Kind != nil {
		k := string(*in.Kind)
		p.Kind = &k
	}
	t, err := s.Process.Edit(r.Context(), fromUUID(taskId), p, ActorFromContext(r.Context()))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTask(t))
}

// CloseTask finishes a task wherever it stands. Whatever an agent is still doing on it is
// stopped: closing says the work is no longer wanted, and an agent carrying on would spend
// money on it and then try to hand in work nobody can advance. Refusing to close until a
// person had found and stopped each job would only make the same thing take longer.
func (s *Server) CloseTask(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.TaskStateChange
	if !decode(w, r, &in) {
		return
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		writeError(w, http.StatusBadRequest, "closing a task needs a reason", "invalid")
		return
	}
	t, err := s.Process.Close(r.Context(), fromUUID(taskId), ActorFromContext(r.Context()), reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.stopJobs(r, t.ID, "task closed: "+reason)
	writeJSON(w, http.StatusOK, toTask(t))
}

// stopJobs stops every job still queued or running on a task. Best effort: the task is
// already closed, and a job that cannot be stopped now finds it closed when it reports back.
func (s *Server) stopJobs(r *http.Request, taskID pgtype.UUID, reason string) {
	if s.Runners == nil {
		return
	}
	jobs, err := db.New(s.Pool).ListActiveJobsForTask(r.Context(), taskID)
	if err != nil {
		s.Log.Warn("listing jobs of a closed task", "err", err)
		return
	}
	for _, j := range jobs {
		if err := s.Runners.StopJob(r.Context(), j.ID, reason); err != nil {
			s.Log.Warn("stopping a job of a closed task", "job", toUUID(j.ID).String(), "err", err)
		}
	}
}

// ReopenTask brings closed work back where it was.
func (s *Server) ReopenTask(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.TaskStateChange
	if !decode(w, r, &in) {
		return
	}
	t, err := s.Process.Reopen(r.Context(), fromUUID(taskId), ActorFromContext(r.Context()), strings.TrimSpace(in.Reason))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTask(t))
}
