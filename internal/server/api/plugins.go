package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/plugins"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

func (s *Server) pluginsOff(w http.ResponseWriter) bool {
	if s.Plugins == nil {
		writeError(w, http.StatusServiceUnavailable, "plugins are not enabled on this server", "unavailable")
		return true
	}
	return false
}

func (s *Server) pluginErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, plugins.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
	case errors.Is(err, plugins.ErrNoKeyring):
		writeError(w, http.StatusConflict, err.Error(), "conflict")
	default:
		s.fail(w, err)
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func toInstalled(p db.Plugin) gen.InstalledPlugin {
	var m plugin.Manifest
	_ = json.Unmarshal(p.Manifest, &m)
	var raw map[string]interface{}
	_ = json.Unmarshal(p.Manifest, &raw)
	return gen.InstalledPlugin{Name: p.Name, Version: p.Version, Command: orEmpty(p.Command), Capabilities: orEmpty(m.Capabilities),
		Hooks: orEmpty(m.Hooks), Enabled: p.Enabled, Manifest: &raw}
}

func toProjectPlugin(v plugins.View) gen.ProjectPlugin {
	out := gen.ProjectPlugin{Name: v.Name, Version: v.Version, Enabled: v.Enabled, DisabledReason: v.DisabledReason, Running: v.Running,
		Capabilities: orEmpty(v.Capabilities), Hooks: orEmpty(v.Hooks), Config: v.Config, SecretsSet: orEmpty(v.SecretsSet)}
	if out.Config == nil {
		out.Config = map[string]interface{}{}
	}
	if len(v.UI) > 0 {
		ui := v.UI
		out.Ui = &ui
	}
	if len(v.ConfigSchema) > 0 {
		var m map[string]interface{}
		if json.Unmarshal(v.ConfigSchema, &m) == nil {
			out.ConfigSchema = &m
		}
	}
	return out
}

func (s *Server) ListPlugins(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok || s.pluginsOff(w) {
		return
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "plugins are managed by workspace admins", "forbidden")
		return
	}
	rows, err := s.Plugins.Plugins(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.InstalledPlugin, 0, len(rows))
	for _, row := range rows {
		out = append(out, toInstalled(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) InstallPlugin(w http.ResponseWriter, r *http.Request, pluginName gen.PluginName) {
	p, ok := s.principal(w, r)
	if !ok || s.pluginsOff(w) {
		return
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "plugins are installed by workspace admins", "forbidden")
		return
	}
	var in gen.InstallPlugin
	if !decode(w, r, &in) {
		return
	}
	if _, err := s.Plugins.Install(r.Context(), string(pluginName), in.Command); err != nil {
		s.pluginErr(w, err)
		return
	}
	row, err := db.New(s.Pool).GetPluginByName(r.Context(), string(pluginName))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toInstalled(row))
}

func (s *Server) ListProjectPlugins(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok || s.pluginsOff(w) {
		return
	}
	views, err := s.Plugins.ProjectPlugins(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.ProjectPlugin, 0, len(views))
	for _, v := range views {
		out = append(out, toProjectPlugin(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ConfigureProjectPlugin(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, pluginName gen.PluginName) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok || s.pluginsOff(w) {
		return
	}
	var in gen.ProjectPluginSettings
	if !decode(w, r, &in) {
		return
	}
	// No config at all means "only switch it on or off". A client toggling a plugin must not
	// have to resend every setting, and until now an empty config did exactly that: it wiped
	// the public settings, or failed validation when one was required.
	settings := plugins.Settings{Enabled: in.Enabled, KeepConfig: in.Config == nil}
	if in.Config != nil {
		settings.Config = *in.Config
	}
	v, err := s.Plugins.Configure(r.Context(), fromUUID(projectId), string(pluginName), settings)
	if err != nil {
		s.pluginErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toProjectPlugin(v))
}

func (s *Server) ListPluginCalls(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, pluginName gen.PluginName, params gen.ListPluginCallsParams) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok || s.pluginsOff(w) {
		return
	}
	limit := 100
	if params.Limit != nil {
		limit = *params.Limit
	}
	rows, err := s.Plugins.Calls(r.Context(), fromUUID(projectId), string(pluginName), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.PluginCall, 0, len(rows))
	for _, row := range rows {
		c := gen.PluginCall{Id: row.ID, Direction: row.Direction, Method: row.Method, Error: row.Error,
			DurationMs: int(row.DurationMs), CreatedAt: row.CreatedAt.Time}
		if len(row.Payload) > 0 {
			var v interface{}
			_ = json.Unmarshal(row.Payload, &v)
			c.Payload = &v
		}
		if len(row.Result) > 0 {
			var v interface{}
			_ = json.Unmarshal(row.Result, &v)
			c.Result = &v
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) GetTaskPluginUI(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleViewer); !ok || s.pluginsOff(w) {
		return
	}
	t, err := db.New(s.Pool).GetTask(r.Context(), fromUUID(taskId))
	if err != nil {
		s.fail(w, err)
		return
	}
	tabs, chips := s.Plugins.RenderUI(r.Context(), t)
	out := gen.PluginUI{Tabs: []gen.PluginTab{}, Chips: []gen.PluginChip{}}
	for _, x := range tabs {
		out.Tabs = append(out.Tabs, gen.PluginTab{Plugin: x.Plugin, Title: x.Title, Markdown: x.Markdown})
	}
	for _, x := range chips {
		c := gen.PluginChip{Plugin: x.Plugin, Text: x.Text}
		if x.Color != "" {
			c.Color = &x.Color
		}
		if x.URL != "" {
			c.Url = &x.URL
		}
		out.Chips = append(out.Chips, c)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ListProjectTemplates(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	views, err := s.Process.Templates(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.ProjectTemplate, 0, len(views))
	for _, v := range views {
		source := gen.ProjectTemplateSource(v.Source)
		t := gen.ProjectTemplate{Kind: v.Kind, MaxRollbacks: v.MaxRollbacks, Phases: []gen.TemplatePhase{}, Source: &source}
		for _, p := range v.Phases {
			timeout := p.Timeout
			phase := gen.TemplatePhase{Name: p.Name, Owner: p.Owner, Gate: p.Gate, Active: p.Active, Timeout: &timeout}
			if p.Role != "" {
				phase.Role = &p.Role
			}
			if p.Requires != "" {
				phase.Requires = &p.Requires
			}
			t.Phases = append(t.Phases, phase)
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, out)
}
