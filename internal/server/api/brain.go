package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/quality"
	"github.com/onegator/gator/internal/server/store/db"
)

// The Brain's REST doors, for people. Who reads and writes what is brain.Policy's, the same rules
// the MCP server and the package a job is handed follow.

// brainReader is the person calling, as the Brain's rules see them; it answers the caller itself
// when there is none.
func (s *Server) brainReader(w http.ResponseWriter, r *http.Request) (brain.Reader, bool) {
	if s.Brain == nil {
		writeError(w, http.StatusServiceUnavailable, "the Brain is not enabled on this server", "unavailable")
		return brain.Reader{}, false
	}
	p, ok := s.principal(w, r)
	if !ok {
		return brain.Reader{}, false
	}
	if p.Kind != auth.KindUser {
		writeError(w, http.StatusForbidden, "the Brain's API is for people; a job reads it through /agent and /mcp", "forbidden")
		return brain.Reader{}, false
	}
	reader, err := s.Brain.Readers.ForPrincipal(r.Context(), p)
	if err != nil {
		s.fail(w, err)
		return brain.Reader{}, false
	}
	return reader, true
}

// failBrain answers what the Brain refused in the words of HTTP.
func (s *Server) failBrain(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, brain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found", "not_found")
	case errors.Is(err, brain.ErrForbidden):
		writeError(w, http.StatusForbidden, "you may not write that here", "forbidden")
	case brain.IsInvalid(err):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
	default:
		s.fail(w, err)
	}
}

func (s *Server) SearchBrain(w http.ResponseWriter, r *http.Request, params gen.SearchBrainParams) {
	reader, ok := s.brainReader(w, r)
	if !ok {
		return
	}
	q := brain.Query{Text: deref(params.Q)}
	if params.Scope != nil {
		q.Scopes = []string{string(*params.Scope)}
	}
	if params.Kind != nil {
		q.Kinds = []string{string(*params.Kind)}
	}
	if params.Status != nil {
		q.Statuses = []string{string(*params.Status)}
	}
	if params.Project != nil {
		q.Project = fromUUID(*params.Project)
	}
	if params.Limit != nil {
		q.Limit = *params.Limit
	}
	if params.At != nil {
		q.At = *params.At
	}
	hits, err := s.Brain.Finder.Find(r.Context(), reader, q)
	if err != nil {
		s.failBrain(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(hits, func(h brain.Hit) gen.BrainHit { return toBrainHit(brain.Summarize(h)) }))
}

func (s *Server) CreateBrainEntry(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.brainReader(w, r)
	if !ok {
		return
	}
	var in gen.NewBrainEntry
	if !decode(w, r, &in) {
		return
	}
	d := brain.Draft{Scope: string(in.Scope), Kind: string(in.Kind), Title: in.Title, Content: in.Content,
		Component: deref(in.ComponentKey), Propose: in.Propose != nil && *in.Propose}
	if in.ProjectId != nil {
		d.Project = fromUUID(*in.ProjectId)
	}
	if in.AgentId != nil {
		d.Agent = fromUUID(*in.AgentId)
	}
	if in.Topic != nil {
		d.Topic = string(*in.Topic)
	}
	if in.Visibility != nil {
		d.Visibility = string(*in.Visibility)
	}
	if d.Scope == brain.ScopeUser {
		d.User = reader.User // a person writes their own context, and nobody else's
	}
	if in.Links != nil {
		for _, l := range *in.Links {
			link := brain.Link{TargetKind: string(l.TargetKind), TargetID: l.TargetId}
			if l.Relation != nil {
				link.Relation = string(*l.Relation)
			}
			d.Links = append(d.Links, link)
		}
	}
	e, err := s.Brain.Service.Write(r.Context(), reader, d)
	if err != nil {
		s.failBrain(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toBrainEntry(e))
}

func (s *Server) ListBrainProposals(w http.ResponseWriter, r *http.Request, params gen.ListBrainProposalsParams) {
	reader, ok := s.brainReader(w, r)
	if !ok {
		return
	}
	var project pgtype.UUID
	if params.Project != nil {
		project = fromUUID(*params.Project)
	}
	rows, err := s.Brain.Service.Proposals(r.Context(), reader, project)
	if err != nil {
		s.failBrain(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, toBrainEntry))
}

func (s *Server) GetBrainEntry(w http.ResponseWriter, r *http.Request, brainEntryId gen.BrainEntryId) {
	reader, ok := s.brainReader(w, r)
	if !ok {
		return
	}
	d, err := s.Brain.Service.Get(r.Context(), reader, fromUUID(brainEntryId))
	if err != nil {
		s.failBrain(w, err)
		return
	}
	// Every version is shown only as far as the reader may read it: an earlier version that came
	// from a private conversation stays with that conversation's readers.
	versions, err := s.Brain.Service.Visible(r.Context(), reader, d.Versions)
	if err != nil {
		s.failBrain(w, err)
		return
	}
	out := gen.BrainDetail{Entry: toBrainEntry(d.Entry), Versions: mapAll(versions, toBrainEntry),
		Links: mapAll(d.Links, toBrainLink), Citations: mapAll(d.Citations, toBrainCitation)}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ReviseBrainEntry(w http.ResponseWriter, r *http.Request, brainEntryId gen.BrainEntryId) {
	reader, ok := s.brainReader(w, r)
	if !ok {
		return
	}
	var in gen.BrainPatch
	if !decode(w, r, &in) {
		return
	}
	c := brain.Change{Title: in.Title, Content: in.Content, Propose: in.Propose != nil && *in.Propose}
	if in.Visibility != nil {
		v := string(*in.Visibility)
		c.Visibility = &v
	}
	e, err := s.Brain.Service.Revise(r.Context(), reader, fromUUID(brainEntryId), c)
	if err != nil {
		s.failBrain(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBrainEntry(e))
}

func (s *Server) ApproveBrainEntry(w http.ResponseWriter, r *http.Request, brainEntryId gen.BrainEntryId) {
	s.decideBrain(w, r, brainEntryId, brain.VerdictApprove)
}

func (s *Server) RejectBrainEntry(w http.ResponseWriter, r *http.Request, brainEntryId gen.BrainEntryId) {
	s.decideBrain(w, r, brainEntryId, brain.VerdictReject)
}

func (s *Server) ArchiveBrainEntry(w http.ResponseWriter, r *http.Request, brainEntryId gen.BrainEntryId) {
	s.decideBrain(w, r, brainEntryId, brain.VerdictArchive)
}

func (s *Server) decideBrain(w http.ResponseWriter, r *http.Request, id gen.BrainEntryId, verdict string) {
	reader, ok := s.brainReader(w, r)
	if !ok {
		return
	}
	e, err := s.Brain.Service.Decide(r.Context(), reader, fromUUID(id), verdict)
	if err != nil {
		s.failBrain(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBrainEntry(e))
}

func toBrainEntry(e db.BrainEntry) gen.BrainEntry {
	src := brain.SourceOf(e)
	out := gen.BrainEntry{
		Id: toUUID(e.ID), Lineage: toUUID(e.Lineage), Scope: gen.BrainScope(e.Scope), ProjectId: toUUIDPtr(e.ProjectID),
		ComponentKey: e.ComponentKey, UserId: toUUIDPtr(e.UserID), AgentId: toUUIDPtr(e.AgentID), Kind: gen.BrainKind(e.Kind),
		Topic: e.Topic, Title: e.Title, Content: e.Content, Visibility: gen.BrainVisibility(e.Visibility),
		Status: gen.BrainStatus(e.Status), Version: int(e.Version), Supersedes: toUUIDPtr(e.Supersedes),
		ValidFrom: e.ValidFrom.Time, ValidUntil: timePtr(e.ValidUntil), Confidence: gen.BrainEntryConfidence(e.Confidence),
		Source: toBrainSource(src), SourceTaskId: toUUIDPtr(e.SourceTaskID), SourceConversationId: toUUIDPtr(e.SourceConversationID),
		Private: e.SourcePrivate, CreatedByKind: e.CreatedByKind, CreatedBy: toUUIDPtr(e.CreatedBy),
		ApprovedBy: toUUIDPtr(e.ApprovedBy), ApprovedAt: timePtr(e.ApprovedAt), ArchivedAt: timePtr(e.ArchivedAt),
		CreatedAt: e.CreatedAt.Time, UpdatedAt: e.UpdatedAt.Time,
	}
	return out
}

func toBrainSource(s brain.Source) gen.BrainSource {
	return gen.BrainSource{Kind: s.Kind, Id: optionalString(s.ID), Url: optionalString(s.URL),
		Excerpt: optionalString(s.Excerpt), Name: optionalString(s.Name)}
}

func toBrainHit(s brain.Summary) gen.BrainHit {
	out := gen.BrainHit{Title: s.Title, Excerpt: s.Excerpt, Scope: gen.BrainScope(s.Scope), Kind: gen.BrainKind(s.Kind),
		Status: gen.BrainStatus(s.Status), Version: s.Version, Source: toBrainSource(s.Source), Rank: &s.Rank,
		ComponentKey: optionalString(s.ComponentKey)}
	if u, ok := brain.ParseUUID(s.ID); ok {
		out.Id = toUUID(u)
	}
	if u, ok := brain.ParseUUID(s.ProjectID); ok {
		out.ProjectId = toUUIDPtr(u)
	}
	return out
}

func toBrainLink(l db.BrainLink) gen.BrainLink {
	return gen.BrainLink{TargetKind: gen.BrainLinkTargetKind(l.TargetKind), TargetId: l.TargetID, Relation: gen.BrainLinkRelation(l.Relation),
		CreatedAt: l.CreatedAt.Time}
}

func toBrainCitation(c db.ListBrainCitationsRow) gen.BrainCitation {
	return gen.BrainCitation{JobId: toUUID(c.JobID), TaskId: toUUID(c.TaskID), Role: c.Role,
		How: gen.BrainCitationHow(c.How), At: c.CreatedAt.Time}
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GetBrainImpact answers whether the Brain helps this project, from its own numbers.
func (s *Server) GetBrainImpact(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, params gen.GetBrainImpactParams) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	if s.Quality == nil {
		writeError(w, http.StatusServiceUnavailable, "measurements are not enabled on this server", "unavailable")
		return
	}
	days := 30
	if params.Days != nil {
		days = *params.Days
	}
	impact, err := s.Quality.BrainImpact(r.Context(), fromUUID(projectId), params.Pivot, time.Duration(days)*24*time.Hour)
	if err != nil {
		s.fail(w, err)
		return
	}
	sample := func(x quality.Sample) gen.BrainSample {
		return gen.BrainSample{Jobs: x.Jobs, MedianContextBytes: x.MedianContextBytes, Advances: x.Advances,
			Rollbacks: x.Rollbacks, RollbackRate: x.RollbackRate}
	}
	writeJSON(w, http.StatusOK, gen.BrainImpact{Before: sample(impact.Before), After: sample(impact.After),
		ContextGrowth: impact.ContextGrowth, ContextHeld: impact.ContextHeld, RollbacksFell: impact.RollbacksFell})
}

// agentsSee is what a person is told agents see of their context.
const agentsSee = "An agent sees your private entries (and your focus) only in a turn you asked for, directly or through " +
	"agents it asked. Entries you share with a project are seen by its people and by every agent working in it; " +
	"nothing of yours is kept in the record of what a job was handed."

func (s *Server) GetMyBrain(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.brainReader(w, r)
	if !ok {
		return
	}
	rows, err := s.Brain.Service.Mine(r.Context(), reader)
	if err != nil {
		s.failBrain(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.MyBrain{Entries: mapAll(rows, toBrainEntry), AgentsSee: agentsSee})
}

func (s *Server) ForgetMyBrain(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.brainReader(w, r)
	if !ok {
		return
	}
	n, err := s.Brain.Service.Forget(r.Context(), reader)
	if err != nil {
		s.failBrain(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.Forgotten{Deleted: int(n)})
}

func (s *Server) ListBrainToReview(w http.ResponseWriter, r *http.Request, params gen.ListBrainToReviewParams) {
	reader, ok := s.brainReader(w, r)
	if !ok {
		return
	}
	days := 90
	if params.Days != nil {
		days = *params.Days
	}
	var project pgtype.UUID
	if params.Project != nil {
		project = fromUUID(*params.Project)
	}
	rows, err := s.Brain.Service.ToReview(r.Context(), reader, project, time.Now().Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		s.failBrain(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, toBrainEntry))
}
