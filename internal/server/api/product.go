package api

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/store/db"
)

// The product context's doors, kept for one release over the Brain: a project's entries of the
// product kinds, in the words they always had ("principles", three statuses).

func toProductEntry(e db.BrainEntry) gen.ProductEntry {
	out := gen.ProductEntry{Id: toUUID(e.ID), Kind: gen.ProductEntryKind(brain.ProductKind(e.Kind)), Title: e.Title,
		Content: e.Content, Version: int(e.Version), Status: gen.ProductEntryStatus(brain.ProductStatus(e.Status)),
		CreatedAt: e.CreatedAt.Time, SourceTaskId: toUUIDPtr(e.SourceTaskID), ApprovedAt: timePtr(e.ApprovedAt)}
	return out
}

func (s *Server) ListProductContext(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	rows, err := s.Brain.Legacy.Product(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, toProductEntry))
}

// AddProductEntry writes what a person says about the product; it needs no approval.
func (s *Server) AddProductEntry(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleMember); !ok {
		return
	}
	reader, ok := s.brainReader(w, r)
	if !ok {
		return
	}
	var in gen.NewProductEntry
	if !decode(w, r, &in) {
		return
	}
	if in.Title == "" || in.Content == "" {
		writeError(w, http.StatusBadRequest, "title and content are required", "invalid")
		return
	}
	entry, err := s.Brain.Service.Write(r.Context(), reader, brain.Draft{Scope: brain.ScopeProject, Project: fromUUID(projectId),
		Kind: brain.KindOfProduct(string(in.Kind)), Title: in.Title, Content: in.Content})
	if err != nil {
		s.failBrain(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toProductEntry(entry))
}

// SetProductEntryStatus approves a proposal or archives an entry. Approving a condensed entry
// archives the entries it replaces: still readable, no longer carried into every prompt.
func (s *Server) SetProductEntryStatus(w http.ResponseWriter, r *http.Request, entryId openapi_types.UUID) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	entry, err := db.New(s.Pool).GetBrainEntry(r.Context(), fromUUID(entryId))
	if err != nil {
		s.fail(w, err)
		return
	}
	if entry.Scope != brain.ScopeProject {
		writeError(w, http.StatusNotFound, "not found", "not_found")
		return
	}
	if _, ok := s.requireProject(w, r, entry.ProjectID, auth.RoleMember); !ok {
		return
	}
	var in gen.ProductStatusChange
	if !decode(w, r, &in) {
		return
	}
	verdict := brain.VerdictArchive
	if in.Status == gen.ProductStatusChangeStatusApproved {
		verdict = brain.VerdictApprove
	}
	updated, err := s.Brain.Service.DecideAs(r.Context(), entry.ID, verdict, p.UserID)
	if err != nil {
		s.failBrain(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toProductEntry(updated))
}
