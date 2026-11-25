package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/onegator/gator/internal/server/agents"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// agentsCaller is the caller of an agents endpoint that is switched on.
func (s *Server) agentsCaller(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if s.Agents == nil {
		writeError(w, http.StatusServiceUnavailable, "agents are not enabled on this server", "unavailable")
		return auth.Principal{}, false
	}
	return s.principal(w, r)
}

// failAgents answers what the agents package refused.
func (s *Server) failAgents(w http.ResponseWriter, err error) {
	trim := func(e error) string { return strings.TrimPrefix(err.Error(), e.Error()+": ") }
	switch {
	case errors.Is(err, agents.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found", "not_found")
	case errors.Is(err, agents.ErrForbidden):
		writeError(w, http.StatusForbidden, trim(agents.ErrForbidden), "forbidden")
	case errors.Is(err, agents.ErrInvalid):
		writeError(w, http.StatusBadRequest, trim(agents.ErrInvalid), "invalid")
	case errors.Is(err, agents.ErrConflict):
		writeError(w, http.StatusConflict, trim(agents.ErrConflict), "conflict")
	default:
		s.failConversation(w, err)
	}
}

// GetTurn answers one turn for anyone who may read where it answers.
func (s *Server) GetTurn(w http.ResponseWriter, r *http.Request, turnId gen.TurnId) {
	p, ok := s.agentsCaller(w, r)
	if !ok {
		return
	}
	turn, _, err := s.Agents.Control.Turn(r.Context(), p, fromUUID(turnId))
	if err != nil {
		s.failAgents(w, err)
		return
	}
	s.respondTurn(w, r, turn, http.StatusOK)
}

// StopTurn stops an answer being written.
func (s *Server) StopTurn(w http.ResponseWriter, r *http.Request, turnId gen.TurnId) {
	p, ok := s.agentsCaller(w, r)
	if !ok {
		return
	}
	var in gen.Reason
	if r.ContentLength > 0 && !decode(w, r, &in) {
		return
	}
	turn, err := s.Agents.Control.Stop(r.Context(), p, fromUUID(turnId), deref(in.Reason))
	if err != nil {
		s.failAgents(w, err)
		return
	}
	s.respondTurn(w, r, turn, http.StatusAccepted)
}

func (s *Server) respondTurn(w http.ResponseWriter, r *http.Request, t db.AgentTurn, status int) {
	var cost db.JobCostRow
	if t.JobID.Valid {
		var err error
		if cost, err = db.New(s.Pool).JobCost(r.Context(), t.JobID); err != nil {
			s.fail(w, err)
			return
		}
	}
	writeJSON(w, status, toAgentTurn(t, cost))
}

func toAgentTurn(t db.AgentTurn, cost db.JobCostRow) gen.AgentTurn {
	return gen.AgentTurn{
		Id: toUUID(t.ID), AgentId: toUUID(t.AgentID), ProjectId: toUUID(t.ProjectID), ConversationId: toUUID(t.ConversationID),
		TriggerMessageId: toUUIDPtr(t.TriggerMessageID), ReplyMessageId: toUUIDPtr(t.ReplyMessageID),
		RequestedByKind: gen.AgentTurnRequestedByKind(t.RequestedByKind), RequestedBy: toUUIDPtr(t.RequestedBy),
		OnBehalfOf: toUUIDPtr(t.OnBehalfOf), ChainId: toUUID(t.ChainID), ParentTurnId: toUUIDPtr(t.ParentTurnID),
		EscalatedFrom: toUUIDPtr(t.EscalatedFrom), Hop: int(t.Hop), Tier: gen.AgentTurnTier(t.Tier), JobId: toUUIDPtr(t.JobID),
		Status: gen.AgentTurnStatus(t.Status), Refusal: t.Refusal, CostUsd: cost.CostUsd, DurationMs: cost.DurationMs,
		CreatedAt: t.CreatedAt.Time, FinishedAt: timePtr(t.FinishedAt),
	}
}

// AllowTurnToWork is the consent card's button: agents are allowed on the task and the agent
// takes a work turn on the same question.
func (s *Server) AllowTurnToWork(w http.ResponseWriter, r *http.Request, turnId gen.TurnId) {
	p, ok := s.agentsCaller(w, r)
	if !ok {
		return
	}
	turn, err := s.Agents.Consent.Allow(r.Context(), p, fromUUID(turnId))
	if err != nil {
		s.failAgents(w, err)
		return
	}
	s.respondTurn(w, r, turn, http.StatusCreated)
}
