package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/a2a"
	"github.com/onegator/gator/internal/server/agents"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// ListProjectAgents answers the agents a project's people can mention.
func (s *Server) ListProjectAgents(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	p, ok := s.agentsCaller(w, r)
	if !ok {
		return
	}
	list, err := s.Agents.Directory.List(r.Context(), p, fromUUID(projectId))
	if err != nil {
		s.failAgents(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(list, toAgent))
}

// CreateProjectAgent makes a project's own agent.
func (s *Server) CreateProjectAgent(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	s.createAgent(w, r, fromUUID(projectId))
}

// ListWorkspaceAgents answers the workspace's agents.
func (s *Server) ListWorkspaceAgents(w http.ResponseWriter, r *http.Request) {
	p, ok := s.agentsCaller(w, r)
	if !ok {
		return
	}
	list, err := s.Agents.Directory.ListWorkspace(r.Context(), p)
	if err != nil {
		s.failAgents(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(list, toAgent))
}

// CreateWorkspaceAgent makes an agent every project sees.
func (s *Server) CreateWorkspaceAgent(w http.ResponseWriter, r *http.Request) {
	s.createAgent(w, r, pgtype.UUID{})
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request, project pgtype.UUID) {
	p, ok := s.agentsCaller(w, r)
	if !ok {
		return
	}
	var in gen.NewAgent
	if !decode(w, r, &in) {
		return
	}
	d := agents.Draft{Handle: in.Handle, DisplayName: in.DisplayName, Description: deref(in.Description), Avatar: deref(in.Avatar),
		Instructions: deref(in.Instructions), RoleKey: in.RoleKey, Policy: fromAgentPolicy(in.Policy)}
	if in.Tier != nil {
		d.Tier = string(*in.Tier)
	}
	a, err := s.Agents.Directory.Create(r.Context(), p, project, d)
	if err != nil {
		s.failAgents(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAgent(a))
}

// GetAgent answers an agent's profile.
func (s *Server) GetAgent(w http.ResponseWriter, r *http.Request, agentId gen.AgentId) {
	p, ok := s.agentsCaller(w, r)
	if !ok {
		return
	}
	a, st, err := s.Agents.Directory.Stats(r.Context(), p, fromUUID(agentId))
	if err != nil {
		s.failAgents(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.AgentProfile{Agent: toAgent(a), Stats: gen.AgentStats{Since: st.Since, Turns: st.Turns,
		Refused: st.Refused, Jobs: st.Jobs, Done: st.Done, Ended: st.Ended, DoneShare: st.DoneShare(), CostUsd: st.CostUSD}})
}

// UpdateAgent changes or pauses an agent.
func (s *Server) UpdateAgent(w http.ResponseWriter, r *http.Request, agentId gen.AgentId) {
	p, ok := s.agentsCaller(w, r)
	if !ok {
		return
	}
	var in gen.AgentPatch
	if !decode(w, r, &in) {
		return
	}
	patch := agents.Patch{Handle: in.Handle, DisplayName: in.DisplayName, Description: in.Description, Avatar: in.Avatar,
		Instructions: in.Instructions, RoleKey: in.RoleKey, Policy: fromAgentPolicy(in.Policy), Skills: in.Skills, Tools: in.Tools}
	if in.Tier != nil {
		patch.Tier = new(string(*in.Tier))
	}
	if in.Status != nil {
		patch.Status = new(string(*in.Status))
	}
	a, err := s.Agents.Directory.Update(r.Context(), p, fromUUID(agentId), patch)
	if err != nil {
		s.failAgents(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAgent(a))
}

// DeleteAgent removes an agent a person made.
func (s *Server) DeleteAgent(w http.ResponseWriter, r *http.Request, agentId gen.AgentId) {
	p, ok := s.agentsCaller(w, r)
	if !ok {
		return
	}
	if err := s.Agents.Directory.Delete(r.Context(), p, fromUUID(agentId)); err != nil {
		s.failAgents(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListAgentTurns answers an agent's history.
func (s *Server) ListAgentTurns(w http.ResponseWriter, r *http.Request, agentId gen.AgentId, params gen.ListAgentTurnsParams) {
	p, ok := s.agentsCaller(w, r)
	if !ok {
		return
	}
	since := time.Now().Add(-30 * 24 * time.Hour)
	if params.Since != nil {
		since = *params.Since
	}
	limit := 0
	if params.Limit != nil {
		limit = *params.Limit
	}
	turns, err := s.Agents.Directory.Turns(r.Context(), p, fromUUID(agentId), since, limit)
	if err != nil {
		s.failAgents(w, err)
		return
	}
	q := db.New(s.Pool)
	out := make([]gen.AgentTurn, 0, len(turns))
	for _, t := range turns {
		var cost db.JobCostRow
		if t.JobID.Valid {
			if cost, err = q.JobCost(r.Context(), t.JobID); err != nil {
				s.fail(w, err)
				return
			}
		}
		out = append(out, toAgentTurn(t, cost))
	}
	writeJSON(w, http.StatusOK, out)
}

func toAgent(a db.Agent) gen.Agent {
	out := gen.Agent{Id: toUUID(a.ID), ProjectId: toUUIDPtr(a.ProjectID), Handle: a.Handle, DisplayName: a.DisplayName,
		Description: a.Description, Avatar: a.Avatar, RoleKey: a.RoleKey, Instructions: a.Instructions, Tier: gen.AgentTier(a.Tier),
		Status: gen.AgentStatus(a.Status), Builtin: agents.SourceOf(a) == agents.SourceBuiltin, CreatedBy: toUUIDPtr(a.CreatedBy),
		CreatedAt: a.CreatedAt.Time, UpdatedAt: a.UpdatedAt.Time, Tools: []string{}, Skills: []string{}, Source: map[string]any{}}
	pol := agents.PolicyOf(a)
	out.Policy = gen.AgentPolicy{Backend: optString(pol.Backend), Model: optString(pol.Model)}
	if pol.MaxCostUSD > 0 {
		out.Policy.MaxCostUsd = &pol.MaxCostUSD
	}
	if pol.DailyBudgetUSD > 0 {
		out.Policy.DailyBudgetUsd = &pol.DailyBudgetUSD
	}
	_ = json.Unmarshal(a.Tools, &out.Tools)
	_ = json.Unmarshal(a.Skills, &out.Skills)
	_ = json.Unmarshal(a.Source, &out.Source)
	kind := gen.AgentKind(a.Kind)
	out.Kind, out.A2aExposed = &kind, &a.A2aExposed
	if a.Kind == agents.KindRemote {
		info := a2a.InfoOf(a)
		ri := gen.RemoteAgentInfo{CardUrl: info.CardURL, Url: info.URL, Name: info.Name, Description: optString(info.Description),
			Version: optString(info.Version), HasCredential: a.RemoteSecrets != ""}
		if !info.FetchedAt.IsZero() {
			ri.FetchedAt = &info.FetchedAt
		}
		skills := make([]gen.A2ASkill, 0, len(info.Skills))
		for _, sk := range info.Skills {
			skills = append(skills, gen.A2ASkill{Id: sk.ID, Name: sk.Name, Description: sk.Description})
		}
		ri.Skills = &skills
		out.Remote = &ri
	}
	return out
}

func fromAgentPolicy(p *gen.AgentPolicy) *agents.Policy {
	if p == nil {
		return nil
	}
	out := &agents.Policy{RolePolicy: process.RolePolicy{Backend: deref(p.Backend), Model: deref(p.Model)}}
	if p.MaxCostUsd != nil {
		out.MaxCostUSD = *p.MaxCostUsd
	}
	if p.DailyBudgetUsd != nil {
		out.DailyBudgetUSD = *p.DailyBudgetUsd
	}
	return out
}
