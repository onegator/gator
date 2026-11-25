package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/review"
)

// reviewOn answers 503 when the server keeps no checkpoints.
func (s *Server) reviewOn(w http.ResponseWriter) bool {
	if s.Review == nil {
		writeError(w, http.StatusServiceUnavailable, "checkpoints are not enabled on this server", "unavailable")
		return false
	}
	return true
}

// failReview answers what the review package refused.
func (s *Server) failReview(w http.ResponseWriter, err error) {
	trim := func(e error) string { return strings.TrimPrefix(err.Error(), e.Error()+": ") }
	switch {
	case errors.Is(err, review.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found", "not_found")
	case errors.Is(err, review.ErrForbidden):
		writeError(w, http.StatusForbidden, trim(review.ErrForbidden), "forbidden")
	case errors.Is(err, review.ErrInvalid):
		writeError(w, http.StatusBadRequest, trim(review.ErrInvalid), "invalid")
	default:
		s.fail(w, err)
	}
}

// ListJobCheckpoints answers what each turn of a job changed, for whoever may read the job.
func (s *Server) ListJobCheckpoints(w http.ResponseWriter, r *http.Request, jobId gen.JobId) {
	if !s.reviewOn(w) {
		return
	}
	if _, ok := s.requireJob(w, r, jobId, auth.RoleViewer); !ok {
		return
	}
	cps, err := s.Review.Checkpoints.List(r.Context(), fromUUID(jobId))
	if err != nil {
		s.failReview(w, err)
		return
	}
	out := make([]gen.Checkpoint, 0, len(cps))
	for _, cp := range cps {
		out = append(out, toCheckpoint(cp))
	}
	writeJSON(w, http.StatusOK, out)
}

// GetJobDiff answers a job's diff over some of its turns.
func (s *Server) GetJobDiff(w http.ResponseWriter, r *http.Request, jobId gen.JobId, params gen.GetJobDiffParams) {
	if !s.reviewOn(w) {
		return
	}
	if _, ok := s.requireJob(w, r, jobId, auth.RoleViewer); !ok {
		return
	}
	diff, err := s.Review.Checkpoints.JobDiff(r.Context(), fromUUID(jobId), deref(params.Repo), derefInt(params.From), derefInt(params.To))
	if err != nil {
		s.failReview(w, err)
		return
	}
	writeDiff(w, diff, params.IgnoreWhitespace)
}

// GetCheckpointDiff answers one turn's diff, or the job's up to it.
func (s *Server) GetCheckpointDiff(w http.ResponseWriter, r *http.Request, checkpointId gen.CheckpointId, params gen.GetCheckpointDiffParams) {
	if !s.reviewOn(w) {
		return
	}
	cp, err := s.Review.Checkpoints.Get(r.Context(), fromUUID(checkpointId))
	if err != nil {
		s.failReview(w, err)
		return
	}
	if _, ok := s.requireJob(w, r, toUUID(cp.JobID), auth.RoleViewer); !ok {
		return
	}
	which := review.TurnDiff
	if params.Whole != nil && *params.Whole {
		which = review.WholeDiff
	}
	diff, err := s.Review.Checkpoints.Diff(r.Context(), cp.ID, which)
	if err != nil {
		s.failReview(w, err)
		return
	}
	writeDiff(w, diff, params.IgnoreWhitespace)
}

// writeDiff answers a diff as text, never as a page: it is the agent's words and the repository's.
func writeDiff(w http.ResponseWriter, diff []byte, ignoreWhitespace *bool) {
	if ignoreWhitespace != nil && *ignoreWhitespace {
		diff = review.IgnoreWhitespace(diff)
	}
	w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(diff)
}

func toCheckpoint(cp review.Checkpoint) gen.Checkpoint {
	return gen.Checkpoint{Id: toUUID(cp.ID), JobId: toUUID(cp.JobID), TurnId: toUUIDPtr(cp.TurnID), Seq: int(cp.Seq), Repo: cp.Repo,
		BaseCommit: cp.BaseCommit, HeadCommit: cp.HeadCommit, Files: toFileChanges(cp.Files), DiffBytes: cp.DiffBytes,
		Truncated: cp.Truncated, WholeFiles: toFileChanges(cp.WholeFiles), WholeBytes: cp.WholeBytes, WholeTruncated: cp.WholeTruncated,
		TookMs: int(cp.TookMs), CreatedAt: cp.CreatedAt.Time}
}

func toFileChanges(b []byte) []gen.FileChange {
	var files []proto.FileChange
	_ = json.Unmarshal(b, &files)
	out := make([]gen.FileChange, 0, len(files))
	for _, f := range files {
		fc := gen.FileChange{Path: f.Path, Added: f.Added, Removed: f.Removed, Status: gen.FileChangeStatus(f.Status)}
		if f.Binary {
			fc.Binary = &f.Binary
		}
		out = append(out, fc)
	}
	return out
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
