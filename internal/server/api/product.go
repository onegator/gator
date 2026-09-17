package api

import (
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

func toProductEntry(e db.ProductContext) gen.ProductEntry {
	out := gen.ProductEntry{Id: toUUID(e.ID), Kind: gen.ProductEntryKind(e.Kind), Title: e.Title, Content: e.Content,
		Version: int(e.Version), Status: gen.ProductEntryStatus(e.Status), CreatedAt: e.CreatedAt.Time}
	out.SourceTaskId = toUUIDPtr(e.SourceTaskID)
	if e.ApprovedAt.Valid {
		at := e.ApprovedAt.Time
		out.ApprovedAt = &at
	}
	return out
}

func (s *Server) ListProductContext(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListProductContext(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.ProductEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, toProductEntry(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// AddProductEntry writes what a person says about the product; it needs no approval.
func (s *Server) AddProductEntry(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	p, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleMember)
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
	entry, err := db.New(s.Pool).CreateProductEntry(r.Context(), db.CreateProductEntryParams{
		ProjectID: fromUUID(projectId), Kind: string(in.Kind), Title: in.Title, Content: in.Content,
		Status: "approved", CreatedByKind: string(auth.KindUser), CreatedBy: p.UserID,
		ApprovedBy: p.UserID, ApprovedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toProductEntry(entry))
}

// SetProductEntryStatus approves a proposal or archives an entry.
func (s *Server) SetProductEntryStatus(w http.ResponseWriter, r *http.Request, entryId openapi_types.UUID) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	q := db.New(s.Pool)
	entry, err := q.GetProductEntry(r.Context(), fromUUID(entryId))
	if err != nil {
		s.fail(w, err)
		return
	}
	if _, ok := s.requireProject(w, r, entry.ProjectID, auth.RoleMember); !ok {
		return
	}
	var in gen.ProductStatusChange
	if !decode(w, r, &in) {
		return
	}
	params := db.SetProductEntryStatusParams{ID: entry.ID, Status: string(in.Status)}
	if string(in.Status) == "approved" {
		params.ApprovedBy = p.UserID
	}
	updated, err := q.SetProductEntryStatus(r.Context(), params)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toProductEntry(updated))
}
