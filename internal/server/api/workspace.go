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

// GetWorkspaceProcess returns the process this workspace has decided for every project in it.
// A project's own process_config still wins; this is what applies without one.
func (s *Server) GetWorkspaceProcess(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.FromContext(r.Context()); !ok {
		writeError(w, http.StatusUnauthorized, "sign in", "unauthorized")
		return
	}
	ws, err := db.New(s.Pool).GetWorkspaceSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.ProjectConfig{Config: rawConfig(ws.ProcessConfig)})
}

// SetWorkspaceProcess replaces it, refusing a template that would not hold. One bad template
// here would otherwise break every project that does not override that kind of task.
func (s *Server) SetWorkspaceProcess(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok || !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "the workspace process is set by workspace admins", "forbidden")
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
	ws, err := db.New(s.Pool).SetWorkspaceProcessConfig(r.Context(), raw)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.ProjectConfig{Config: rawConfig(ws.ProcessConfig)})
}
