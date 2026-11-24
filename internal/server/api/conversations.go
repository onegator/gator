package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
)

// conversationsOff answers 503 when the server runs without conversations.
func (s *Server) conversationsOff(w http.ResponseWriter) bool {
	if s.Conversations == nil {
		writeError(w, http.StatusServiceUnavailable, "conversations are not enabled on this server", "unavailable")
		return true
	}
	return false
}

// failConversation maps the conversations package's errors to responses. A conversation the
// caller may not read answers 404, like one that does not exist.
func (s *Server) failConversation(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, conversations.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error(), "forbidden")
	case errors.Is(err, conversations.ErrInvalid):
		writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), conversations.ErrInvalid.Error()+": "), "invalid")
	case errors.Is(err, conversations.ErrConflict):
		writeError(w, http.StatusConflict, strings.TrimPrefix(err.Error(), conversations.ErrConflict.Error()+": "), "conflict")
	default:
		s.fail(w, err)
	}
}

// conversationCaller is the caller, for a conversations endpoint that is switched on.
func (s *Server) conversationCaller(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if s.conversationsOff(w) {
		return auth.Principal{}, false
	}
	return s.principal(w, r)
}

func (s *Server) ListProjectConversations(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, params gen.ListProjectConversationsParams) {
	if s.conversationsOff(w) {
		return
	}
	p, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer)
	if !ok {
		return
	}
	kind := conversations.KindChannel
	if params.Kind != nil {
		kind = string(*params.Kind)
	}
	rows, err := s.Conversations.Directory.ListProject(r.Context(), p, fromUUID(projectId), kind, params.Archived != nil && *params.Archived)
	if err != nil {
		s.failConversation(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, toConversation))
}

func (s *Server) CreateChannel(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if s.conversationsOff(w) {
		return
	}
	p, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer)
	if !ok {
		return
	}
	var in gen.NewChannel
	if !decode(w, r, &in) {
		return
	}
	nc := conversations.NewChannel{Name: in.Name, Topic: deref(in.Topic), ComponentKey: deref(in.ComponentKey)}
	if in.Visibility != nil {
		nc.Visibility = string(*in.Visibility)
	}
	conv, err := s.Conversations.Directory.CreateChannel(r.Context(), p, fromUUID(projectId), nc)
	if err != nil {
		s.failConversation(w, err)
		return
	}
	s.respondConversation(w, r, p, conv.ID, http.StatusCreated)
}

func (s *Server) ListMyConversations(w http.ResponseWriter, r *http.Request, params gen.ListMyConversationsParams) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	kind := ""
	if params.Kind != nil {
		kind = string(*params.Kind)
	}
	rows, err := s.Conversations.Directory.ListMine(r.Context(), p, kind)
	if err != nil {
		s.failConversation(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, toConversation))
}

func (s *Server) OpenDirectMessage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	var in gen.NewDirectMessage
	if !decode(w, r, &in) {
		return
	}
	people := make([]pgtype.UUID, 0, len(in.Members))
	for _, m := range in.Members {
		people = append(people, fromUUID(m))
	}
	var project pgtype.UUID
	if in.ProjectId != nil {
		project = fromUUID(*in.ProjectId)
	}
	conv, created, err := s.Conversations.Directory.OpenDM(r.Context(), p, people, project)
	if err != nil {
		s.failConversation(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.respondConversation(w, r, p, conv.ID, status)
}

func (s *Server) GetConversation(w http.ResponseWriter, r *http.Request, conversationId gen.ConversationId) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	s.respondConversation(w, r, p, fromUUID(conversationId), http.StatusOK)
}

func (s *Server) UpdateConversation(w http.ResponseWriter, r *http.Request, conversationId gen.ConversationId) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	var in gen.ConversationPatch
	if !decode(w, r, &in) {
		return
	}
	if _, err := s.Conversations.Directory.Update(r.Context(), p, fromUUID(conversationId), conversations.Patch{
		Name: in.Name, Topic: in.Topic, Archived: in.Archived,
	}); err != nil {
		s.failConversation(w, err)
		return
	}
	s.respondConversation(w, r, p, fromUUID(conversationId), http.StatusOK)
}

func (s *Server) SetConversationMember(w http.ResponseWriter, r *http.Request, conversationId gen.ConversationId,
	memberKind gen.SetConversationMemberParamsMemberKind, memberId gen.MemberId) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	var in gen.ConversationMemberChange
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	change := conversations.MemberChange{}
	if in.Notify != nil {
		change.Notify = ptr(string(*in.Notify))
	}
	if in.Role != nil {
		change.Role = ptr(string(*in.Role))
	}
	m, err := s.Conversations.Directory.SetMember(r.Context(), p, fromUUID(conversationId),
		conversations.Member{Kind: string(memberKind), ID: fromUUID(memberId)}, change)
	if err != nil {
		s.failConversation(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toConversationMember(m.MemberKind, m.MemberID, "", m.Role, m.Notify, m.LastReadSeq, m.JoinedAt))
}

func (s *Server) RemoveConversationMember(w http.ResponseWriter, r *http.Request, conversationId gen.ConversationId,
	memberKind gen.RemoveConversationMemberParamsMemberKind, memberId gen.MemberId) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	if err := s.Conversations.Directory.RemoveMember(r.Context(), p, fromUUID(conversationId),
		conversations.Member{Kind: string(memberKind), ID: fromUUID(memberId)}); err != nil {
		s.failConversation(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ListMessages(w http.ResponseWriter, r *http.Request, conversationId gen.ConversationId, params gen.ListMessagesParams) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	page := conversations.Page{}
	if params.BeforeSeq != nil {
		page.BeforeSeq = *params.BeforeSeq
	}
	page.AfterSeq = params.AfterSeq
	if params.Limit != nil {
		page.Limit = *params.Limit
	}
	res, err := s.Conversations.Messages.List(r.Context(), p, fromUUID(conversationId), page)
	if err != nil {
		s.failConversation(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.MessagePage{Messages: mapAll(res.Messages, toMessage), HasMore: res.HasMore})
}

func (s *Server) PostMessage(w http.ResponseWriter, r *http.Request, conversationId gen.ConversationId) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	var in gen.NewMessage
	if !decode(w, r, &in) {
		return
	}
	posted, err := s.Conversations.Messages.Post(r.Context(), p, fromUUID(conversationId), toDraft(in))
	if err != nil {
		s.failConversation(w, err)
		return
	}
	status := http.StatusCreated
	if posted.Replayed {
		status = http.StatusOK
	}
	s.respondMessage(w, r, p, posted.Message.ID, status)
}

func (s *Server) MarkConversationRead(w http.ResponseWriter, r *http.Request, conversationId gen.ConversationId) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	var in gen.ReadMarker
	if !decode(w, r, &in) {
		return
	}
	m, err := s.Conversations.Messages.MarkRead(r.Context(), p, fromUUID(conversationId), in.Seq)
	if err != nil {
		s.failConversation(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toConversationMember(m.MemberKind, m.MemberID, "", m.Role, m.Notify, m.LastReadSeq, m.JoinedAt))
}

func (s *Server) GetMessage(w http.ResponseWriter, r *http.Request, messageId gen.MessageId) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	s.respondMessage(w, r, p, fromUUID(messageId), http.StatusOK)
}

func (s *Server) GetMessageThread(w http.ResponseWriter, r *http.Request, messageId gen.MessageId) {
	p, ok := s.conversationCaller(w, r)
	if !ok {
		return
	}
	thread, err := s.Conversations.Threads.Of(r.Context(), p, fromUUID(messageId))
	if err != nil {
		s.failConversation(w, err)
		return
	}
	s.respondConversation(w, r, p, thread.ID, http.StatusOK)
}

// respondConversation answers a conversation as the caller sees it.
func (s *Server) respondConversation(w http.ResponseWriter, r *http.Request, p auth.Principal, id pgtype.UUID, status int) {
	d, err := s.Conversations.Directory.Get(r.Context(), p, id)
	if err != nil {
		s.failConversation(w, err)
		return
	}
	writeJSON(w, status, toConversationDetail(d))
}

// respondMessage answers one message as the caller sees it.
func (s *Server) respondMessage(w http.ResponseWriter, r *http.Request, p auth.Principal, id pgtype.UUID, status int) {
	v, err := s.Conversations.Messages.Get(r.Context(), p, id)
	if err != nil {
		s.failConversation(w, err)
		return
	}
	writeJSON(w, status, toMessage(v))
}

func toDraft(in gen.NewMessage) conversations.Draft {
	d := conversations.Draft{Body: deref(in.Body), ClientID: deref(in.ClientId)}
	if in.Mentions != nil {
		for _, m := range *in.Mentions {
			d.Mentions = append(d.Mentions, conversations.Member{Kind: string(m.Kind), ID: fromUUID(m.Id)})
		}
	}
	return d
}

func ptr[T any](v T) *T { return &v }
