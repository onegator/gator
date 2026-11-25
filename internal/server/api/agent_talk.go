package api

import (
	"net/http"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/conversations"
)

// The agent's side of a conversation: what a running job may say in the conversation its turn
// answers in. Like the rest of the agent surface, every call is written into the job's events.

// AgentSay says something in the job's conversation as its agent.
func (s *Server) AgentSay(w http.ResponseWriter, r *http.Request) {
	job, ok := s.agentJob(w, r, cmdSay)
	if !ok || !s.agentsOn(w) {
		return
	}
	var in gen.AgentSay
	if !decode(w, r, &in) {
		return
	}
	msg, err := s.Agents.Talk.Say(r.Context(), job, in.Body)
	if err != nil {
		s.failAgents(w, err)
		return
	}
	s.note(r, job, "say", map[string]any{"message": toUUID(msg.ID).String()})
	writeJSON(w, http.StatusOK, gen.AgentWrite{Ok: true, Id: new(toUUID(msg.ID)), Link: new(conversations.MessageLink(msg)),
		Detail: new("said in the conversation")})
}

// AgentThread reads more of the job's conversation than its prompt carried.
func (s *Server) AgentThread(w http.ResponseWriter, r *http.Request, params gen.AgentThreadParams) {
	job, ok := s.agentJob(w, r, cmdThreadShow)
	if !ok || !s.agentsOn(w) {
		return
	}
	limit := 50
	if params.Limit != nil {
		limit = min(max(*params.Limit, 1), 200)
	}
	conv, said, marker, err := s.Agents.Talk.Thread(r.Context(), job, limit)
	if err != nil {
		s.failAgents(w, err)
		return
	}
	out := gen.AgentThread{ConversationId: toUUID(conv.ID), Messages: make([]gen.AgentThreadMessage, 0, len(said))}
	for _, m := range said {
		body := m.Body
		if m.Untrusted {
			body = marker.Fence(m.Who, m.Body) // fenced as the prompt fences them (a nonce of its own), not a flag a model may skip
		}
		out.Messages = append(out.Messages, gen.AgentThreadMessage{Seq: m.Seq, Author: m.Who, Body: body, Untrusted: m.Untrusted})
	}
	s.note(r, job, "thread show", map[string]any{"messages": len(said)})
	writeJSON(w, http.StatusOK, out)
}

// AgentProposeTask proposes a task; a person accepts it or not.
func (s *Server) AgentProposeTask(w http.ResponseWriter, r *http.Request) {
	job, ok := s.agentJob(w, r, cmdTaskCreate)
	if !ok || !s.agentsOn(w) {
		return
	}
	var in gen.AgentTaskProposal
	if !decode(w, r, &in) {
		return
	}
	msg, err := s.Agents.Talk.ProposeTask(r.Context(), job, string(in.Kind), in.Title, deref(in.Description))
	if err != nil {
		s.failAgents(w, err)
		return
	}
	s.note(r, job, "task create", map[string]any{"kind": in.Kind, "title": in.Title})
	writeJSON(w, http.StatusOK, gen.AgentWrite{Ok: true, Id: new(toUUID(msg.ID)), Link: new(conversations.MessageLink(msg)),
		Detail: new("proposed; a person makes it a task or not")})
}

// AcceptTaskProposal makes the task an agent proposed.
func (s *Server) AcceptTaskProposal(w http.ResponseWriter, r *http.Request, messageId gen.MessageId) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	if s.Conversations.Proposals == nil {
		writeError(w, http.StatusServiceUnavailable, "making tasks from conversations is not enabled on this server", "unavailable")
		return
	}
	task, err := s.Conversations.Proposals.Accept(r.Context(), p, ActorFromContext(r.Context()), fromUUID(messageId))
	if err != nil {
		s.failConversation(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toTask(task))
}

func (s *Server) agentsOn(w http.ResponseWriter) bool {
	if s.Agents == nil {
		writeError(w, http.StatusServiceUnavailable, "agents are not enabled on this server", "unavailable")
		return false
	}
	return true
}
