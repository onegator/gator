package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/automations"
)

func (s *Server) automationsOff(w http.ResponseWriter) bool {
	if s.Automations == nil {
		writeError(w, http.StatusServiceUnavailable, "automations are not enabled on this server", "unavailable")
		return true
	}
	return false
}

// ListProjectAutomations is the catalogue with the project's state. Anyone who can read the
// project sees it, so people know what runs on their behalf.
func (s *Server) ListProjectAutomations(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok || s.automationsOff(w) {
		return
	}
	views, err := s.Automations.List(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.Automation, 0, len(views))
	for _, v := range views {
		out = append(out, toAutomation(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// SetProjectAutomation switches one on or off. Only a person who is a project admin may: switching
// one on gives consent on behalf of every task it will create, which no agent or plugin can give.
func (s *Server) SetProjectAutomation(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, automationKey gen.AutomationKey) {
	p, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin)
	if !ok || s.automationsOff(w) {
		return
	}
	if p.Kind != auth.KindUser || !p.UserID.Valid {
		writeError(w, http.StatusForbidden, "only a person can switch automations on or off", "forbidden")
		return
	}
	var in gen.AutomationChange
	if !decode(w, r, &in) {
		return
	}
	change := automations.Change{Enabled: in.Enabled, AllowAgents: in.AllowAgents != nil && *in.AllowAgents}
	if in.Settings != nil {
		change.Settings = &automations.Settings{Weekday: deref(in.Settings.Weekday)}
	}
	v, err := s.Automations.Set(r.Context(), fromUUID(projectId), automationKey, change, p.UserID)
	switch {
	case errors.Is(err, automations.ErrUnknown):
		writeError(w, http.StatusNotFound, "no automation "+automationKey, "not_found")
	case errors.Is(err, automations.ErrConsentRequired):
		writeError(w, http.StatusConflict, err.Error(), "consent_required")
	case errors.Is(err, automations.ErrUnavailable):
		writeError(w, http.StatusConflict, strings.TrimPrefix(err.Error(), automations.ErrUnavailable.Error()+": "), "automation_unavailable")
	case errors.Is(err, automations.ErrInvalidSettings):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
	case err != nil:
		s.fail(w, err)
	default:
		writeJSON(w, http.StatusOK, toAutomation(v))
	}
}

func toAutomation(v automations.View) gen.Automation {
	out := gen.Automation{Key: v.Key, Title: v.Title, Summary: v.Summary, Creates: v.Creates, Trigger: v.Trigger, Needs: v.Needs,
		UsesAgents: v.UsesAgents, Available: v.Available, UnavailableReason: optString(v.Unavailable), Enabled: v.Enabled,
		Settings:  gen.AutomationSettings{Weekday: optString(v.Settings.Weekday)},
		EnabledBy: toUUIDPtr(v.EnabledBy), EnabledByName: optString(v.EnabledName), EnabledAt: timePtr(v.EnabledAt),
		LastRunAt: timePtr(v.LastRunAt), LastError: optString(v.LastError), LastTaskId: toUUIDPtr(v.LastTaskID),
		NextRunAt: timePtr(v.NextRunAt)}
	return out
}
