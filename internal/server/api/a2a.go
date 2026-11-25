package api

import (
	"errors"
	"net/http"
	"strings"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/a2a"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

func (s *Server) a2aCaller(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if s.A2A == nil {
		writeError(w, http.StatusServiceUnavailable, "A2A is not configured on this server", "unavailable")
		return auth.Principal{}, false
	}
	return s.principal(w, r)
}

func (s *Server) failA2A(w http.ResponseWriter, err error) {
	trim := func(e error) string { return strings.TrimPrefix(err.Error(), e.Error()+": ") }
	switch {
	case errors.Is(err, a2a.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found", "not_found")
	case errors.Is(err, a2a.ErrForbidden):
		writeError(w, http.StatusForbidden, "not allowed", "forbidden")
	case errors.Is(err, a2a.ErrInvalid):
		writeError(w, http.StatusBadRequest, trim(a2a.ErrInvalid), "invalid")
	case errors.Is(err, a2a.ErrConflict):
		writeError(w, http.StatusConflict, trim(a2a.ErrConflict), "conflict")
	case errors.Is(err, a2a.ErrUnreachable):
		writeError(w, http.StatusBadGateway, err.Error(), "unreachable")
	case errors.Is(err, a2a.ErrNoKeyring):
		writeError(w, http.StatusServiceUnavailable, err.Error(), "no_keyring")
	default:
		s.fail(w, err)
	}
}

func (s *Server) CreateRemoteAgent(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	p, ok := s.a2aCaller(w, r)
	if !ok {
		return
	}
	var in gen.NewRemoteAgent
	if !decode(w, r, &in) {
		return
	}
	a, err := s.A2A.Remote.Create(r.Context(), p, fromUUID(projectId), a2a.NewRemoteAgent{Handle: strings.TrimPrefix(strings.TrimSpace(in.Handle), "@"),
		CardURL: in.CardUrl, Token: deref(in.Token), Name: deref(in.DisplayName)})
	if err != nil {
		s.failA2A(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAgent(a))
}

func (s *Server) ListA2AClients(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	p, ok := s.a2aCaller(w, r)
	if !ok {
		return
	}
	list, err := s.A2A.Inbound.Clients(r.Context(), p, fromUUID(projectId))
	if err != nil {
		s.failA2A(w, err)
		return
	}
	out := make([]gen.A2AClient, 0, len(list))
	for _, c := range list {
		out = append(out, toA2AClient(c.Client, c.SpentToday))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) CreateA2AClient(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	p, ok := s.a2aCaller(w, r)
	if !ok {
		return
	}
	var in gen.NewA2AClient
	if !decode(w, r, &in) {
		return
	}
	made, err := s.A2A.Inbound.CreateClient(r.Context(), p, fromUUID(projectId), a2a.NewClient{Name: strings.TrimSpace(in.Name),
		DailyBudgetUSD: in.DailyBudgetUsd, PerMinute: in.PerMinute})
	if err != nil {
		s.failA2A(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, gen.A2AClientCreated{Client: toA2AClient(made.Client, 0), Token: made.Token})
}

func (s *Server) RevokeA2AClient(w http.ResponseWriter, r *http.Request, clientId openapi_types.UUID) {
	p, ok := s.a2aCaller(w, r)
	if !ok {
		return
	}
	c, err := s.A2A.Inbound.Revoke(r.Context(), p, fromUUID(clientId))
	if err != nil {
		s.failA2A(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toA2AClient(c, 0))
}

func (s *Server) SetAgentA2A(w http.ResponseWriter, r *http.Request, agentId gen.AgentId) {
	p, ok := s.a2aCaller(w, r)
	if !ok || s.Agents == nil {
		if ok {
			writeError(w, http.StatusServiceUnavailable, "agents are not enabled on this server", "unavailable")
		}
		return
	}
	var in gen.AgentA2A
	if !decode(w, r, &in) {
		return
	}
	a, err := s.Agents.Directory.Get(r.Context(), p, fromUUID(agentId))
	if err != nil {
		s.failAgents(w, err)
		return
	}
	a, err = s.A2A.Inbound.SetExposed(r.Context(), p, a, in.Exposed)
	if err != nil {
		s.failA2A(w, err)
		return
	}
	out := gen.AgentA2AState{Agent: toAgent(a), Exposed: a.A2aExposed}
	if a.A2aExposed {
		if proj, err := db.New(s.Pool).GetProject(r.Context(), a.ProjectID); err == nil {
			url := s.A2A.Inbound.AgentURL(proj.Slug, a.Handle)
			card := url + a2a.CardPath
			out.Url, out.CardUrl = &url, &card
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func toA2AClient(c db.A2aClient, spent float64) gen.A2AClient {
	return gen.A2AClient{Id: toUUID(c.ID), ProjectId: toUUID(c.ProjectID), Name: c.Name, DailyBudgetUsd: c.DailyBudgetUsd,
		PerMinute: int(c.PerMinute), SpentTodayUsd: spent, IdentityId: toUUIDPtr(c.IdentityID), CreatedBy: toUUIDPtr(c.CreatedBy),
		CreatedAt: c.CreatedAt.Time, RevokedAt: timePtr(c.RevokedAt)}
}
