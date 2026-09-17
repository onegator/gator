package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/knowledge"
	"github.com/onegator/gator/internal/server/store/db"
)

func toKnowledgeEntry(e db.KnowledgeEntry) gen.KnowledgeEntry {
	return gen.KnowledgeEntry{Id: toUUID(e.ID), ProjectId: toUUIDPtr(e.ProjectID), Scope: gen.KnowledgeEntryScope(e.Scope),
		Title: e.Title, Content: e.Content, Position: int(e.Position), CreatedAt: e.CreatedAt.Time}
}

func toPack(p db.KnowledgePack, enabled *bool) gen.KnowledgePack {
	var m knowledge.Manifest
	_ = json.Unmarshal(p.Manifest, &m)
	out := gen.KnowledgePack{Id: toUUID(p.ID), Name: p.Name, Version: p.Version, Scope: m.Scope,
		Checksum: p.Checksum, Files: m.Files, Enabled: enabled}
	if p.Url != "" {
		out.Url = &p.Url
	}
	if len(m.AppliesTo) > 0 {
		out.AppliesTo = &m.AppliesTo
	}
	if len(m.Roles) > 0 {
		out.Roles = &m.Roles
	}
	if len(m.Phases) > 0 {
		out.Phases = &m.Phases
	}
	if p.FetchedAt.Valid {
		at := p.FetchedAt.Time
		out.FetchedAt = &at
	}
	if out.Files == nil {
		out.Files = []string{}
	}
	return out
}

func (s *Server) newEntry(w http.ResponseWriter, r *http.Request, project pgtype.UUID, in gen.NewKnowledgeEntry) {
	if in.Title == "" || in.Content == "" {
		writeError(w, http.StatusBadRequest, "title and content are required", "invalid")
		return
	}
	position := int32(100)
	if in.Position != nil {
		position = int32(*in.Position)
	}
	entry, err := db.New(s.Pool).CreateKnowledgeEntry(r.Context(), db.CreateKnowledgeEntryParams{
		ProjectID: project, Scope: string(in.Scope), Title: in.Title, Content: in.Content, Position: position,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toKnowledgeEntry(entry))
}

func (s *Server) ListWorkspaceKnowledge(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.principal(w, r); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListWorkspaceKnowledge(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.KnowledgeEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, toKnowledgeEntry(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) AddWorkspaceKnowledge(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "workspace knowledge is written by workspace admins", "forbidden")
		return
	}
	var in gen.NewKnowledgeEntry
	if !decode(w, r, &in) {
		return
	}
	s.newEntry(w, r, pgtype.UUID{}, in)
}

func (s *Server) DeleteKnowledgeEntry(w http.ResponseWriter, r *http.Request, entryId openapi_types.UUID) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	q := db.New(s.Pool)
	entry, err := q.GetKnowledgeEntry(r.Context(), fromUUID(entryId))
	if err != nil {
		s.fail(w, err)
		return
	}
	if entry.ProjectID.Valid {
		if _, ok := s.requireProject(w, r, entry.ProjectID, auth.RoleAdmin); !ok {
			return
		}
	} else if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "workspace knowledge is managed by workspace admins", "forbidden")
		return
	}
	if _, err := q.DeleteKnowledgeEntry(r.Context(), entry.ID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ListProjectKnowledge(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListProjectKnowledge(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.KnowledgeEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, toKnowledgeEntry(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) AddProjectKnowledge(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleMember); !ok {
		return
	}
	var in gen.NewKnowledgeEntry
	if !decode(w, r, &in) {
		return
	}
	s.newEntry(w, r, fromUUID(projectId), in)
}

func (s *Server) ListKnowledgePacks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.principal(w, r); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListKnowledgePacks(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.KnowledgePack, 0, len(rows))
	for _, p := range rows {
		out = append(out, toPack(p, nil))
	}
	writeJSON(w, http.StatusOK, out)
}

// FetchKnowledgePacks clones a registry and stores what it promises, refusing any pack whose
// files do not hash to the checksum in its index.
func (s *Server) FetchKnowledgePacks(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "registries are fetched by workspace admins", "forbidden")
		return
	}
	var in gen.FetchPacks
	if !decode(w, r, &in) {
		return
	}
	packs, err := knowledge.Fetch(r.Context(), in.Url)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
		return
	}
	q := db.New(s.Pool)
	out := make([]gen.KnowledgePack, 0, len(packs))
	for _, pack := range packs {
		manifest, _ := json.Marshal(pack.Manifest)
		files, _ := json.Marshal(pack.Files)
		stored, err := q.UpsertKnowledgePack(r.Context(), db.UpsertKnowledgePackParams{
			Name: pack.Manifest.Name, Version: pack.Manifest.Version, Source: "git", Url: in.Url,
			Checksum: pack.Checksum(), Manifest: manifest, Files: files,
		})
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, toPack(stored, nil))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ListProjectPacks(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListPacksForProject(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	enabled := true
	out := make([]gen.KnowledgePack, 0, len(rows))
	for _, row := range rows {
		out = append(out, toPack(db.KnowledgePack{ID: row.ID, Name: row.Name, Version: row.Version, Source: row.Source,
			Url: row.Url, Checksum: row.Checksum, Manifest: row.Manifest, Files: row.Files, FetchedAt: row.FetchedAt}, &enabled))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) SetProjectPack(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	var in gen.ProjectPack
	if !decode(w, r, &in) {
		return
	}
	q := db.New(s.Pool)
	var pack db.KnowledgePack
	var err error
	if in.Version != nil && *in.Version != "" {
		pack, err = q.GetKnowledgePack(r.Context(), db.GetKnowledgePackParams{Name: in.Name, Version: *in.Version})
	} else {
		pack, err = q.GetNewestPackByName(r.Context(), in.Name)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no such pack; fetch its registry first", "not_found")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	position := int32(100)
	if in.Position != nil {
		position = int32(*in.Position)
	}
	if err := q.EnablePackForProject(r.Context(), db.EnablePackForProjectParams{
		ProjectID: fromUUID(projectId), PackID: pack.ID, Position: position}); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) RemoveProjectPack(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, packName string) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	n, err := db.New(s.Pool).DisablePacksByName(r.Context(), db.DisablePacksByNameParams{ProjectID: fromUUID(projectId), Name: packName})
	if err != nil {
		s.fail(w, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "that pack is not on for this project", "not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PreviewKnowledge shows the technical part of a prompt as a job would receive it.
func (s *Server) PreviewKnowledge(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, params gen.PreviewKnowledgeParams) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	phase := ""
	if params.Phase != nil {
		phase = *params.Phase
	}
	docs, warnings, err := s.Process.Knowledge(r.Context(), fromUUID(projectId), params.Role, phase)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := gen.KnowledgePreview{Documents: []gen.PreviewDocument{}, Warnings: warnings}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	for _, d := range docs {
		out.Documents = append(out.Documents, gen.PreviewDocument{Title: d.Title, Scope: d.Phase, Body: d.Body})
		out.Bytes += len(d.Body)
	}
	writeJSON(w, http.StatusOK, out)
}
