package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/marketplace"
	"github.com/onegator/gator/internal/server/store/db"
)

func (s *Server) ListProjectInstallations(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	s.listInstallations(w, r, fromUUID(projectId))
}

func (s *Server) ListWorkspaceInstallations(w http.ResponseWriter, r *http.Request) {
	s.listInstallations(w, r, pgtype.UUID{})
}

func (s *Server) listInstallations(w http.ResponseWriter, r *http.Request, project pgtype.UUID) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	rows, err := s.Marketplace.Installations.List(r.Context(), p, project)
	if err != nil {
		s.failMarketplace(w, err)
		return
	}
	out := make([]gen.Installation, 0, len(rows))
	for _, inst := range rows {
		out = append(out, s.toInstallation(r, p, inst))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) InstallIntoProject(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	s.install(w, r, fromUUID(projectId))
}

func (s *Server) InstallIntoWorkspace(w http.ResponseWriter, r *http.Request) {
	s.install(w, r, pgtype.UUID{})
}

func (s *Server) install(w http.ResponseWriter, r *http.Request, project pgtype.UUID) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	var in gen.NewInstallation
	if !decode(w, r, &in) {
		return
	}
	req := marketplace.Request{Project: project, Kind: string(in.Kind), Name: in.Name, Version: deref(in.Version),
		Accepted: in.AcceptedPermissions, Handle: deref(in.Handle)}
	if in.RegistryId != nil {
		req.Registry = fromUUID(*in.RegistryId)
	}
	inst, err := s.Marketplace.Installations.Install(r.Context(), p, req)
	if err != nil {
		s.failInstall(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.toInstallation(r, p, inst))
}

func (s *Server) GetInstallation(w http.ResponseWriter, r *http.Request, installationId gen.InstallationId) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	inst, err := s.Marketplace.Installations.Get(r.Context(), p, fromUUID(installationId))
	if err != nil {
		s.failMarketplace(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toInstallation(r, p, inst))
}

func (s *Server) RemoveInstallation(w http.ResponseWriter, r *http.Request, installationId gen.InstallationId) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	if err := s.Marketplace.Installations.Remove(r.Context(), p, fromUUID(installationId)); err != nil {
		s.failMarketplace(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// failInstall answers a refused install with what is still to accept.
func (s *Server) failInstall(w http.ResponseWriter, err error) {
	if pe, ok := errors.AsType[*marketplace.PermissionsError](err); ok {
		writeJSON(w, http.StatusConflict, gen.InstallRefusal{Error: err.Error(), Code: "permissions_not_accepted", Permissions: &pe.Missing})
		return
	}
	if le, ok := errors.AsType[*marketplace.LocalChangesError](err); ok {
		changes := toLocalChanges(le.Changes)
		writeJSON(w, http.StatusConflict, gen.InstallRefusal{Error: err.Error(), Code: "local_changes", LocalChanges: &changes})
		return
	}
	s.failMarketplace(w, err)
}

func (s *Server) toInstallation(r *http.Request, p auth.Principal, inst db.Installation) gen.Installation {
	var accepted []string
	_ = json.Unmarshal(inst.AcceptedPermissions, &accepted)
	out := gen.Installation{Id: toUUID(inst.ID), ProjectId: toUUIDPtr(inst.ProjectID), RegistryId: toUUID(inst.RegistryID),
		Kind: gen.InstallationKind(inst.Kind), Name: inst.Name, Version: inst.Version, Checksum: inst.Checksum,
		TargetId: toUUIDPtr(inst.TargetID), AcceptedPermissions: orEmpty(accepted), InstalledBy: toUUID(inst.InstalledBy),
		InstalledAt: inst.InstalledAt.Time, UpdatedAt: inst.UpdatedAt.Time}
	if newest, ok := s.Marketplace.Installations.Newer(r.Context(), p, inst); ok {
		out.UpdateAvailable = &newest
	}
	if inst.Kind == marketplace.KindAgent && inst.TargetID.Valid {
		if a, err := db.New(s.Pool).GetAgent(r.Context(), inst.TargetID); err == nil && !a.DeletedAt.Valid {
			agent := toAgent(a)
			out.Agent = &agent
		}
	}
	return out
}

// withProjectState says what a project has installed of a package and what it lacks to run it;
// nothing without a project.
func (s *Server) withProjectState(r *http.Request, p auth.Principal, project pgtype.UUID, m marketplace.Manifest, installed **gen.InstalledRef, missing **[]string) {
	if !project.Valid {
		return
	}
	state, err := s.Marketplace.Installations.State(r.Context(), p, project, m)
	if err != nil {
		s.Log.Warn("marketplace: a project's state of a package", "package", m.Kind+"/"+m.Name, "err", err)
		return
	}
	if i := state.Installed; i != nil {
		workspace := i.Workspace
		*installed = &gen.InstalledRef{InstallationId: toUUID(i.ID), Version: i.Version, UpdateAvailable: i.UpdateAvailable, Workspace: &workspace}
	}
	if len(state.Missing) > 0 {
		*missing = &state.Missing
	}
}

// ListProjectSkills answers the skills the project's agents may carry.
func (s *Server) ListProjectSkills(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListSkillsVisibleIn(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, toSkill))
}

func toSkill(sk db.Skill) gen.SkillInfo {
	out := gen.SkillInfo{Id: toUUID(sk.ID), ProjectId: toUUIDPtr(sk.ProjectID), Name: sk.Name, Version: sk.Version,
		Description: sk.Description, Files: []string{}, Source: map[string]any{}}
	var files marketplace.Files
	_ = json.Unmarshal(sk.Files, &files)
	for _, f := range files {
		out.Files = append(out.Files, f.Path)
	}
	_ = json.Unmarshal(sk.Source, &out.Source)
	return out
}

func (s *Server) UpdateInstallation(w http.ResponseWriter, r *http.Request, installationId gen.InstallationId) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	var in gen.InstallationUpdate
	if !decode(w, r, &in) {
		return
	}
	req := marketplace.UpdateRequest{Version: deref(in.Version)}
	if in.AcceptedPermissions != nil {
		req.Accepted = *in.AcceptedPermissions
	}
	if in.Resolve != nil {
		req.Resolve = string(*in.Resolve)
	}
	inst, err := s.Marketplace.Installations.Update(r.Context(), p, fromUUID(installationId), req)
	if err != nil {
		s.failInstall(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toInstallation(r, p, inst))
}

func (s *Server) PreviewInstallationUpdate(w http.ResponseWriter, r *http.Request, installationId gen.InstallationId, params gen.PreviewInstallationUpdateParams) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	_, pv, err := s.Marketplace.Installations.Preview(r.Context(), p, fromUUID(installationId), deref(params.Version))
	if err != nil {
		s.failMarketplace(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.UpdatePreview{From: pv.From, To: pv.To, AddedPermissions: orEmpty(pv.Added), RemovedPermissions: orEmpty(pv.Removed),
		NeedsAcceptance: len(pv.Added) > 0, LocalChanges: toLocalChanges(pv.Local)})
}

func toLocalChanges(cs []marketplace.LocalChange) []gen.LocalChange {
	out := make([]gen.LocalChange, 0, len(cs))
	for _, c := range cs {
		out = append(out, gen.LocalChange{Part: c.Part, Local: c.Local, Incoming: c.Incoming})
	}
	return out
}
