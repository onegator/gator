package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

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
		TookMs: int(cp.TookMs), CreatedAt: cp.CreatedAt.Time, Attempt: ptr(int(cp.Attempt)), FilesTruncated: &cp.FilesTruncated,
		WholeFilesTruncated: &cp.WholeFilesTruncated}
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

// commentsOn answers 503 when the server takes no comments.
func (s *Server) commentsOn(w http.ResponseWriter) bool {
	if !s.reviewOn(w) {
		return false
	}
	if s.Review.Comments == nil {
		writeError(w, http.StatusServiceUnavailable, "review comments are not enabled on this server", "unavailable")
		return false
	}
	return true
}

// CreateReviewComment keeps a draft comment, for a member of the job's project who may read it.
func (s *Server) CreateReviewComment(w http.ResponseWriter, r *http.Request, checkpointId gen.CheckpointId) {
	if !s.commentsOn(w) {
		return
	}
	cp, err := s.Review.Checkpoints.Get(r.Context(), fromUUID(checkpointId))
	if err != nil {
		s.failReview(w, err)
		return
	}
	if _, ok := s.requireJob(w, r, toUUID(cp.JobID), auth.RoleMember); !ok {
		return
	}
	var in gen.NewReviewComment
	if !decode(w, r, &in) {
		return
	}
	p, _ := auth.FromContext(r.Context())
	c, err := s.Review.Comments.Create(r.Context(), p, cp.ID, review.NewComment{Path: in.Path, Side: string(in.Side),
		LineStart: in.LineStart, LineEnd: derefInt(in.LineEnd), Body: in.Body})
	if err != nil {
		s.failReview(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toReviewComment(c, int(cp.Seq)))
}

// ListJobReviewComments answers the comments to lay over a job's diff.
func (s *Server) ListJobReviewComments(w http.ResponseWriter, r *http.Request, jobId gen.JobId) {
	if !s.commentsOn(w) {
		return
	}
	if _, ok := s.requireJob(w, r, jobId, auth.RoleViewer); !ok {
		return
	}
	p, _ := auth.FromContext(r.Context())
	rows, err := s.Review.Comments.List(r.Context(), p, fromUUID(jobId))
	if err != nil {
		s.failReview(w, err)
		return
	}
	out := make([]gen.ReviewComment, 0, len(rows))
	for _, c := range rows {
		out = append(out, toReviewComment(review.Comment{ID: c.ID, CheckpointID: c.CheckpointID, AuthorID: c.AuthorID, Path: c.Path,
			Side: c.Side, LineStart: c.LineStart, LineEnd: c.LineEnd, Body: c.Body, Snippet: c.Snippet, MessageID: c.MessageID,
			ResolvedAt: c.ResolvedAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}, int(c.CheckpointSeq)))
	}
	writeJSON(w, http.StatusOK, out)
}

// SendReviewComments posts the caller's drafts on a job to its agent.
func (s *Server) SendReviewComments(w http.ResponseWriter, r *http.Request, jobId gen.JobId) {
	if !s.commentsOn(w) || s.conversationsOff(w) {
		return
	}
	job, ok := s.requireJob(w, r, jobId, auth.RoleMember)
	if !ok {
		return
	}
	var in gen.SendReviewComments
	if r.ContentLength > 0 && !decode(w, r, &in) {
		return
	}
	var agent pgtype.UUID
	if in.AgentId != nil {
		agent = fromUUID(*in.AgentId)
	}
	p, _ := auth.FromContext(r.Context())
	posted, sent, err := s.Review.Comments.Send(r.Context(), p, job, agent)
	if err != nil {
		if errors.Is(err, review.ErrInvalid) || errors.Is(err, review.ErrForbidden) {
			s.failReview(w, err)
		} else {
			s.failConversation(w, err)
		}
		return
	}
	v, err := s.Conversations.Messages.Get(r.Context(), p, posted.Message.ID)
	if err != nil {
		s.failConversation(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, gen.SentReviewComments{Message: toMessage(v), Sent: sent})
}

// UpdateReviewComment changes a draft's words, or whether a comment is resolved.
func (s *Server) UpdateReviewComment(w http.ResponseWriter, r *http.Request, commentId gen.CommentId) {
	c, ok := s.reviewComment(w, r, commentId)
	if !ok {
		return
	}
	var in gen.ReviewCommentChange
	if !decode(w, r, &in) {
		return
	}
	p, _ := auth.FromContext(r.Context())
	out, err := s.Review.Comments.Update(r.Context(), p, c.ID, review.Change{Body: in.Body, Resolved: in.Resolved})
	if err != nil {
		s.failReview(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toReviewComment(out, 0))
}

// DeleteReviewComment takes back a draft.
func (s *Server) DeleteReviewComment(w http.ResponseWriter, r *http.Request, commentId gen.CommentId) {
	c, ok := s.reviewComment(w, r, commentId)
	if !ok {
		return
	}
	p, _ := auth.FromContext(r.Context())
	if err := s.Review.Comments.Delete(r.Context(), p, c.ID); err != nil {
		s.failReview(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reviewComment is a comment the caller may change: one they can see, on a job of a project they
// are a member of.
func (s *Server) reviewComment(w http.ResponseWriter, r *http.Request, id gen.CommentId) (review.Comment, bool) {
	if !s.commentsOn(w) {
		return review.Comment{}, false
	}
	p, ok := s.principal(w, r)
	if !ok {
		return review.Comment{}, false
	}
	c, err := s.Review.Comments.Get(r.Context(), p, fromUUID(id))
	if err != nil {
		s.failReview(w, err)
		return review.Comment{}, false
	}
	cp, err := s.Review.Checkpoints.Get(r.Context(), c.CheckpointID)
	if err != nil {
		s.failReview(w, err)
		return review.Comment{}, false
	}
	if _, ok := s.requireJob(w, r, toUUID(cp.JobID), auth.RoleMember); !ok {
		return review.Comment{}, false
	}
	return c, true
}

func toReviewComment(c review.Comment, seq int) gen.ReviewComment {
	state := gen.CommentDraft
	switch {
	case c.ResolvedAt.Valid:
		state = gen.CommentResolved
	case c.MessageID.Valid:
		state = gen.CommentSent
	}
	out := gen.ReviewComment{Id: toUUID(c.ID), CheckpointId: toUUID(c.CheckpointID), AuthorId: toUUID(c.AuthorID), Path: c.Path,
		Side: gen.ReviewCommentSide(c.Side), LineStart: int(c.LineStart), LineEnd: int(c.LineEnd), Body: c.Body, Snippet: c.Snippet,
		MessageId: toUUIDPtr(c.MessageID), State: state, ResolvedAt: timePtr(c.ResolvedAt), CreatedAt: c.CreatedAt.Time,
		UpdatedAt: c.UpdatedAt.Time}
	if seq > 0 {
		out.CheckpointSeq = &seq
	}
	return out
}
