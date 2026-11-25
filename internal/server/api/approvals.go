package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/approvals"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// approvalsCaller is the caller of an approvals endpoint that is switched on.
func (s *Server) approvalsCaller(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if s.Approvals == nil {
		writeError(w, http.StatusServiceUnavailable, "approvals are not enabled on this server", "unavailable")
		return auth.Principal{}, false
	}
	return s.principal(w, r)
}

// failApprovals answers what the approvals package refused.
func (s *Server) failApprovals(w http.ResponseWriter, err error) {
	trim := func(e error) string { return strings.TrimPrefix(err.Error(), e.Error()+": ") }
	switch {
	case errors.Is(err, approvals.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found", "not_found")
	case errors.Is(err, approvals.ErrForbidden):
		writeError(w, http.StatusForbidden, trim(approvals.ErrForbidden), "forbidden")
	case errors.Is(err, approvals.ErrInvalid):
		writeError(w, http.StatusBadRequest, trim(approvals.ErrInvalid), "invalid")
	case errors.Is(err, approvals.ErrConflict):
		writeError(w, http.StatusConflict, trim(approvals.ErrConflict), "conflict")
	default:
		s.failConversation(w, err)
	}
}

// ListWaitingRuntimeRequests answers what waits for the caller to decide.
func (s *Server) ListWaitingRuntimeRequests(w http.ResponseWriter, r *http.Request, params gen.ListWaitingRuntimeRequestsParams) {
	p, ok := s.approvalsCaller(w, r)
	if !ok {
		return
	}
	var project pgtype.UUID
	if params.ProjectId != nil {
		project = fromUUID(*params.ProjectId)
	}
	rows, err := s.Approvals.Waiting(r.Context(), p, project)
	if err != nil {
		s.failApprovals(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRuntimeRequests(rows))
}

// GetRuntimeRequest answers one request.
func (s *Server) GetRuntimeRequest(w http.ResponseWriter, r *http.Request, requestId gen.RequestId) {
	p, ok := s.approvalsCaller(w, r)
	if !ok {
		return
	}
	req, err := s.Approvals.Get(r.Context(), p, fromUUID(requestId))
	if err != nil {
		s.failApprovals(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRuntimeRequest(req))
}

// DecideRuntimeRequest is the card's buttons.
func (s *Server) DecideRuntimeRequest(w http.ResponseWriter, r *http.Request, requestId gen.RequestId) {
	p, ok := s.approvalsCaller(w, r)
	if !ok {
		return
	}
	var in gen.RuntimeDecision
	if !decode(w, r, &in) {
		return
	}
	req, err := s.Approvals.Decide(r.Context(), p, fromUUID(requestId), string(in.Decision), deref(in.Feedback))
	if err != nil {
		s.failApprovals(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRuntimeRequest(req))
}

// ListJobRuntimeRequests answers what a job's agent asked.
func (s *Server) ListJobRuntimeRequests(w http.ResponseWriter, r *http.Request, jobId gen.JobId) {
	if _, ok := s.approvalsCaller(w, r); !ok {
		return
	}
	if _, ok := s.requireJob(w, r, jobId, auth.RoleViewer); !ok {
		return
	}
	rows, err := s.Approvals.ForJob(r.Context(), fromUUID(jobId))
	if err != nil {
		s.failApprovals(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRuntimeRequests(rows))
}

func toRuntimeRequests(rows []db.RuntimeRequest) []gen.RuntimeRequest {
	out := make([]gen.RuntimeRequest, 0, len(rows))
	for _, r := range rows {
		out = append(out, toRuntimeRequest(r))
	}
	return out
}

func toRuntimeRequest(r db.RuntimeRequest) gen.RuntimeRequest {
	out := gen.RuntimeRequest{Id: toUUID(r.ID), JobId: toUUID(r.JobID), TurnId: toUUIDPtr(r.TurnID), Kind: gen.RuntimeRequestKind(r.Kind),
		Tool: r.Tool, Input: objectOf(r.Input), Rule: r.Rule, Status: gen.RuntimeRequestStatus(r.Status), Feedback: r.Feedback,
		DecidedBy: toUUIDPtr(r.DecidedBy), DecidedAt: timePtr(r.DecidedAt), ExpiresAt: r.ExpiresAt.Time, CreatedAt: r.CreatedAt.Time}
	if r.Stopped {
		out.Stopped = &r.Stopped
	}
	return out
}
