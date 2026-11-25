package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/limits"
	"github.com/onegator/gator/internal/server/mcpremote"
	"github.com/onegator/gator/internal/server/store/db"
)

func (s *Server) remoteCaller(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if s.Remote == nil {
		writeError(w, http.StatusServiceUnavailable, "remote MCP servers are not configured on this server", "unavailable")
		return auth.Principal{}, false
	}
	return s.principal(w, r)
}

func (s *Server) failRemote(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mcpremote.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found", "not_found")
	case errors.Is(err, mcpremote.ErrForbidden):
		writeError(w, http.StatusForbidden, "not allowed", "forbidden")
	case errors.Is(err, mcpremote.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
	case errors.Is(err, mcpremote.ErrNoKeyring):
		writeError(w, http.StatusServiceUnavailable, err.Error(), "no_keyring")
	case errors.Is(err, mcpremote.ErrNotConnected), errors.Is(err, limits.ErrOpen):
		writeError(w, http.StatusBadGateway, err.Error(), "unavailable")
	default:
		s.fail(w, err)
	}
}

func (s *Server) ListProjectMcpServers(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	s.listRemote(w, r, fromUUID(projectId))
}

func (s *Server) ListWorkspaceMcpServers(w http.ResponseWriter, r *http.Request) {
	s.listRemote(w, r, pgtype.UUID{})
}

func (s *Server) listRemote(w http.ResponseWriter, r *http.Request, project pgtype.UUID) {
	p, ok := s.remoteCaller(w, r)
	if !ok {
		return
	}
	list, err := s.Remote.Servers.List(r.Context(), p, project)
	if err != nil {
		s.failRemote(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(list, toMcpServer))
}

func (s *Server) GetProjectMcpServer(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, serverName gen.ServerName) {
	p, ok := s.remoteCaller(w, r)
	if !ok {
		return
	}
	v, err := s.Remote.Servers.Get(r.Context(), p, fromUUID(projectId), serverName)
	if err != nil {
		s.failRemote(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toMcpServer(v))
}

func (s *Server) PutProjectMcpServer(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, serverName gen.ServerName) {
	s.putRemote(w, r, fromUUID(projectId), serverName)
}

func (s *Server) PutWorkspaceMcpServer(w http.ResponseWriter, r *http.Request, serverName gen.ServerName) {
	s.putRemote(w, r, pgtype.UUID{}, serverName)
}

func (s *Server) putRemote(w http.ResponseWriter, r *http.Request, project pgtype.UUID, name string) {
	p, ok := s.remoteCaller(w, r)
	if !ok {
		return
	}
	var in gen.McpServerSettings
	if !decode(w, r, &in) {
		return
	}
	set := mcpremote.Settings{DisplayName: in.DisplayName, URL: in.Url, AllowedTools: in.AllowedTools, Enabled: in.Enabled}
	if in.Auth != nil {
		set.Auth = new(string(*in.Auth))
	}
	if in.Secrets != nil {
		set.Secrets = *in.Secrets
	}
	v, err := s.Remote.Servers.Put(r.Context(), p, project, name, set)
	if err != nil {
		s.failRemote(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toMcpServer(v))
}

func (s *Server) ListProjectMcpServerTools(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, serverName gen.ServerName) {
	p, ok := s.remoteCaller(w, r)
	if !ok {
		return
	}
	tools, err := s.Remote.Servers.Tools(r.Context(), p, fromUUID(projectId), serverName)
	if err != nil {
		if errors.Is(err, mcpremote.ErrNotFound) || errors.Is(err, mcpremote.ErrForbidden) {
			s.failRemote(w, err)
			return
		}
		writeError(w, http.StatusBadGateway, "the server did not answer: "+err.Error(), "unavailable")
		return
	}
	out := make([]gen.McpTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, gen.McpTool{Name: t.Name, Description: t.Description, ReadOnly: t.ReadOnly, Allowed: t.Allowed})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ListProjectMcpServerCalls(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, serverName gen.ServerName, params gen.ListProjectMcpServerCallsParams) {
	p, ok := s.remoteCaller(w, r)
	if !ok {
		return
	}
	limit := 100
	if params.Limit != nil {
		limit = *params.Limit
	}
	calls, err := s.Remote.Servers.Calls(r.Context(), p, fromUUID(projectId), serverName, limit)
	if err != nil {
		s.failRemote(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(calls, toMcpCall))
}

func toMcpServer(v mcpremote.View) gen.McpServer {
	srv := v.Server
	out := gen.McpServer{Id: toUUID(srv.ID), ProjectId: toUUIDPtr(srv.ProjectID), Name: srv.Name, DisplayName: srv.DisplayName, Url: srv.Url,
		Auth: gen.McpServerAuth(srv.Auth), AllowedTools: orEmpty(srv.AllowedTools), Enabled: srv.Enabled, Connected: v.Connected,
		SecretsSet: orEmpty(v.SecretsSet), Breaker: gen.McpServerBreaker(v.Breaker), Workspace: !srv.ProjectID.Valid, Source: map[string]any{},
		CreatedAt: srv.CreatedAt.Time, UpdatedAt: srv.UpdatedAt.Time}
	_ = json.Unmarshal(srv.Source, &out.Source)
	return out
}

func toMcpCall(c db.McpCall) gen.McpCall {
	out := gen.McpCall{Id: c.ID, JobId: toUUIDPtr(c.JobID), AgentId: toUUIDPtr(c.AgentID), Method: c.Method, Tool: c.Tool, Outcome: gen.McpCallOutcome(c.Outcome),
		Error: c.Error, ResultBytes: int(c.ResultBytes), DurationMs: int(c.DurationMs), CreatedAt: c.CreatedAt.Time}
	if len(c.Payload) > 0 {
		var m map[string]any
		if json.Unmarshal(c.Payload, &m) == nil {
			out.Payload = &m
		}
	}
	return out
}
