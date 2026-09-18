package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// GetProjectConfig returns process_config as stored.
func (s *Server) GetProjectConfig(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	project, err := db.New(s.Pool).GetProject(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.ProjectConfig{Config: rawConfig(project.ProcessConfig)})
}

// SetProjectConfig replaces the process configuration, refusing one that would not hold:
// a template with no phases, an unknown gate, a phase that no template owns.
func (s *Server) SetProjectConfig(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	var in gen.ProjectConfig
	if !decode(w, r, &in) {
		return
	}
	raw, err := json.Marshal(in.Config)
	if err != nil {
		writeError(w, http.StatusBadRequest, "config must be an object", "invalid")
		return
	}
	cfg, err := process.ParseProjectConfig(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
		return
	}
	for kind, template := range cfg.Templates {
		if template.Kind == "" {
			template.Kind = kind
		}
		if err := template.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("template %s: %v", kind, err), "invalid")
			return
		}
	}
	project, err := db.New(s.Pool).UpdateProjectConfig(r.Context(), db.UpdateProjectConfigParams{ID: fromUUID(projectId), ProcessConfig: raw})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.ProjectConfig{Config: rawConfig(project.ProcessConfig)})
}

func rawConfig(b []byte) map[string]interface{} {
	out := map[string]interface{}{}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// ListProjectMembers says who works on a project and in which role.
func (s *Server) ListProjectMembers(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListProjectMembersWithUsers(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.ProjectMember, 0, len(rows))
	for _, m := range rows {
		out = append(out, gen.ProjectMember{UserId: toUUID(m.ID), Name: m.Name, Email: m.Email, Role: gen.ProjectMemberRole(m.Role)})
	}
	writeJSON(w, http.StatusOK, out)
}

// ListUsers is the workspace's people, for the workspace screen.
func (s *Server) ListUsers(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "the workspace's people are listed by workspace admins", "forbidden")
		return
	}
	rows, err := db.New(s.Pool).ListUsers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.User, 0, len(rows))
	for _, u := range rows {
		out = append(out, gen.User{Id: toUUID(u.ID), Name: u.Name, Email: u.Email,
			WorkspaceRole: gen.UserWorkspaceRole(u.WorkspaceRole), CreatedAt: u.CreatedAt.Time})
	}
	writeJSON(w, http.StatusOK, out)
}
