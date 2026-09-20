package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// preset is the snapshot stored in project_templates.payload. A new product starts from it:
// how its process runs, what its agents read, and what it already believes about itself.
type preset struct {
	Config  json.RawMessage `json:"config,omitempty"`
	Packs   []string        `json:"packs,omitempty"`
	Product []presetEntry   `json:"product,omitempty"`
	Repo    *presetRepo     `json:"repo,omitempty"`
}

type presetEntry struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

type presetRepo struct {
	URL           string `json:"url"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
}

func (s *Server) ListProjectPresets(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.principal(w, r); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListProjectTemplates(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.ProjectPreset, 0, len(rows))
	for _, row := range rows {
		out = append(out, toPreset(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func toPreset(row db.ProjectTemplate) gen.ProjectPreset {
	var p preset
	_ = json.Unmarshal(row.Payload, &p)
	entries := make([]gen.PresetEntry, 0, len(p.Product))
	for _, e := range p.Product {
		entries = append(entries, gen.PresetEntry{Kind: e.Kind, Title: e.Title, Content: e.Content})
	}
	packs := p.Packs
	if packs == nil {
		packs = []string{}
	}
	out := gen.ProjectPreset{Id: toUUID(row.ID), Name: row.Name, Description: row.Description,
		Packs: packs, Product: entries, CreatedAt: row.CreatedAt.Time}
	hasProcess := len(p.Config) > 0 && string(p.Config) != "{}" && string(p.Config) != "null"
	out.HasProcess = &hasProcess
	if p.Repo != nil {
		url := p.Repo.URL
		out.RepoUrl = &url
	}
	return out
}

// SaveProjectPreset snapshots a project: its process, the packs it switched on, the product
// context a person approved, and its primary repository. A snapshot, not a link — a project
// changed tomorrow does not quietly change what the next project starts from.
func (s *Server) SaveProjectPreset(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "presets are saved by workspace admins", "forbidden")
		return
	}
	var in gen.NewProjectPreset
	if !decode(w, r, &in) {
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "a preset needs a name", "invalid")
		return
	}
	ctx := r.Context()
	q := db.New(s.Pool)
	projectID := fromUUID(in.FromProjectId)
	project, err := q.GetProject(ctx, projectID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "no such project", "invalid")
		return
	}
	snapshot := preset{Config: json.RawMessage(project.ProcessConfig)}
	packs, err := q.ListPacksForProject(ctx, projectID)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, pack := range packs {
		snapshot.Packs = append(snapshot.Packs, pack.Name)
	}
	// Only what a person approved: a proposal nobody accepted is not yet how this product
	// thinks, and it certainly should not become how the next one starts.
	entries, err := q.ListApprovedProductContext(ctx, projectID)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, e := range entries {
		snapshot.Product = append(snapshot.Product, presetEntry{Kind: e.Kind, Title: e.Title, Content: e.Content})
	}
	if repo, err := q.GetPrimaryRepo(ctx, projectID); err == nil {
		snapshot.Repo = &presetRepo{URL: repo.Url, DefaultBranch: repo.DefaultBranch}
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		s.fail(w, err)
		return
	}
	row, err := q.UpsertProjectTemplate(ctx, db.UpsertProjectTemplateParams{
		Name: in.Name, Description: deref(in.Description), Payload: payload, CreatedBy: p.UserID})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toPreset(row))
}

func (s *Server) DeleteProjectPreset(w http.ResponseWriter, r *http.Request, presetId openapi_types.UUID) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "presets are removed by workspace admins", "forbidden")
		return
	}
	n, err := db.New(s.Pool).DeleteProjectTemplate(r.Context(), fromUUID(presetId))
	if err != nil {
		s.fail(w, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not found", "not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyPreset fills a fresh project from a preset. Everything is copied: a project must never
// depend on a preset, or on the project the preset was taken from.
func (s *Server) applyPreset(ctx context.Context, name string, project db.Project, by pgtype.UUID) error {
	q := db.New(s.Pool)
	row, err := q.GetProjectTemplateByName(ctx, name)
	if err != nil {
		return fmt.Errorf("no preset named %q", name)
	}
	var p preset
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		return fmt.Errorf("preset %q is unreadable: %w", name, err)
	}
	if len(p.Config) > 0 && string(p.Config) != "null" {
		if _, err := q.UpdateProjectConfig(ctx, db.UpdateProjectConfigParams{ID: project.ID, ProcessConfig: p.Config}); err != nil {
			return err
		}
	}
	for i, name := range p.Packs {
		// A pack the workspace no longer has is skipped rather than fatal: a preset is a
		// starting point, not a promise about what the registry still holds.
		pack, err := q.GetNewestPackByName(ctx, name)
		if err != nil {
			s.Log.Warn("preset names a pack this workspace does not have", "pack", name, "err", err)
			continue
		}
		if err := q.EnablePackForProject(ctx, db.EnablePackForProjectParams{
			ProjectID: project.ID, PackID: pack.ID, Position: int32(100 + i)}); err != nil {
			return err
		}
	}
	for _, e := range p.Product {
		if _, err := q.CreateProductEntry(ctx, db.CreateProductEntryParams{
			ProjectID: project.ID, Kind: e.Kind, Title: e.Title, Content: e.Content,
			Status: "approved", CreatedByKind: "user", CreatedBy: by, ApprovedBy: by,
			ApprovedAt: pgtype.Timestamptz{Time: project.CreatedAt.Time, Valid: true},
		}); err != nil {
			return err
		}
	}
	if p.Repo != nil && p.Repo.URL != "" {
		branch := p.Repo.DefaultBranch
		if branch == "" {
			branch = "main"
		}
		if _, err := q.UpsertProjectRepo(ctx, db.UpsertProjectRepoParams{
			ProjectID: project.ID, Name: project.Slug, Url: p.Repo.URL, DefaultBranch: branch, IsPrimary: true}); err != nil {
			return err
		}
	}
	return nil
}

// ArchiveProject hides a project without losing any of it. Its tasks, receipts and metrics
// stay readable by id; its plugins stop at the next sync, because an archived project should
// not be answering webhooks or spending anyone's budget.
func (s *Server) ArchiveProject(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "archiving a project requires workspace admin", "forbidden")
		return
	}
	var in gen.ArchiveProject
	if !decode(w, r, &in) {
		return
	}
	q := db.New(s.Pool)
	if _, err := q.GetProject(r.Context(), fromUUID(projectId)); err != nil {
		s.fail(w, err)
		return
	}
	updated, err := q.SetProjectArchived(r.Context(), db.SetProjectArchivedParams{ID: fromUUID(projectId), Archived: in.Archived})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toProject(updated))
}

// DeleteProject removes an empty one for good. A project that ever held a task is archived
// instead: its phase transitions and receipts are the record of what happened, and no click
// should be able to erase that.
func (s *Server) DeleteProject(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "deleting a project requires workspace admin", "forbidden")
		return
	}
	q := db.New(s.Pool)
	if _, err := q.GetProject(r.Context(), fromUUID(projectId)); err != nil {
		s.fail(w, err)
		return
	}
	tasks, err := q.CountTasksInProject(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	if tasks > 0 {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("this project holds %d task(s); archive it instead so its history survives", tasks), "conflict")
		return
	}
	if _, err := q.DeleteProject(r.Context(), fromUUID(projectId)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetProjectBudget answers with the numbers the budget gate uses, so a person setting a limit
// sees the same figure that will block the work rather than a different one from a report.
func (s *Server) GetProjectBudget(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	limit, spent, err := s.Process.Budget(r.Context(), fromUUID(projectId), time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	// Cost on a subscription is an estimate; saying so keeps a budget from reading as a bill.
	estimated := spent > 0
	writeJSON(w, http.StatusOK, gen.Budget{DailyLimitUsd: limit, SpentTodayUsd: spent, Estimated: estimated})
}
