package api

import (
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

func toConversation(l conversations.Listed) gen.Conversation {
	c := l.Conversation
	out := gen.Conversation{
		Id: toUUID(c.ID), ProjectId: toUUIDPtr(c.ProjectID), Kind: gen.ConversationKind(c.Kind), Name: c.Name, Topic: c.Topic,
		Visibility: gen.ConversationVisibility(c.Visibility), ParentMessageId: toUUIDPtr(c.ParentMessageID), TaskId: toUUIDPtr(c.TaskID),
		ComponentKey: c.ComponentKey, Archived: c.ArchivedAt.Valid, ArchivedAt: timePtr(c.ArchivedAt), CreatedAt: c.CreatedAt.Time,
		LastMessageAt: timePtr(c.LastMessageAt), LastSeq: c.LastSeq, Joined: l.Joined, LastReadSeq: l.LastReadSeq,
		Unread: l.Unread, UnreadMentions: l.UnreadMentions,
	}
	if l.Notify != "" {
		n := gen.ConversationNotify(l.Notify)
		out.Notify = &n
	}
	return out
}

// toConversationDetail is a conversation with its members and the caller's rights. The caller's
// unread counts come from their own membership.
func toConversationDetail(d conversations.Detail) gen.ConversationDetail {
	l := conversations.Listed{Conversation: d.Conversation}
	if d.Me != nil {
		l.Joined, l.LastReadSeq, l.Notify = true, d.Me.LastReadSeq, d.Me.Notify
		if d.Conversation.LastSeq > d.Me.LastReadSeq {
			l.Unread = d.Conversation.LastSeq - d.Me.LastReadSeq
		}
	}
	c := toConversation(l)
	members := make([]gen.ConversationMember, 0, len(d.Members))
	for _, m := range d.Members {
		members = append(members, toConversationMember(m.MemberKind, m.MemberID, m.Name, m.Role, m.Notify, m.LastReadSeq, m.JoinedAt))
	}
	out := gen.ConversationDetail{
		Id: c.Id, ProjectId: c.ProjectId, Kind: gen.ConversationDetailKind(c.Kind), Name: c.Name, Topic: c.Topic,
		Visibility: gen.ConversationDetailVisibility(c.Visibility), ParentMessageId: c.ParentMessageId, TaskId: c.TaskId,
		ComponentKey: c.ComponentKey, Archived: c.Archived, ArchivedAt: c.ArchivedAt, CreatedAt: c.CreatedAt,
		LastMessageAt: c.LastMessageAt, LastSeq: c.LastSeq, Joined: c.Joined, LastReadSeq: c.LastReadSeq,
		Unread: c.Unread, UnreadMentions: c.UnreadMentions, Members: members,
		Rights: gen.ConversationRights{Read: d.Rights.Read, React: d.Rights.React, Write: d.Rights.Write, Moderate: d.Rights.Moderate},
	}
	if c.Notify != nil {
		n := gen.ConversationDetailNotify(*c.Notify)
		out.Notify = &n
	}
	return out
}

func toConversationMember(kind string, id pgtype.UUID, name, role, notify string, lastRead int64, joined pgtype.Timestamptz) gen.ConversationMember {
	out := gen.ConversationMember{
		Kind: gen.ConversationMemberKind(kind), Id: toUUID(id), Role: gen.ConversationMemberRole(role),
		Notify: gen.ConversationMemberNotify(notify), LastReadSeq: lastRead, JoinedAt: joined.Time,
	}
	if name != "" {
		out.Name = &name
	}
	return out
}

func toMessage(v conversations.View) gen.Message {
	m := v.Message
	out := gen.Message{
		Id: toUUID(m.ID), ConversationId: toUUID(m.ConversationID), Seq: m.Seq, AuthorKind: gen.MessageAuthorKind(m.AuthorKind),
		AuthorId: toUUIDPtr(m.AuthorID), Kind: gen.MessageKind(m.Kind), Body: m.Body, CardType: m.CardType, Card: toCard(m),
		ReplyCount: int(m.ReplyCount), ThreadId: toUUIDPtr(v.ThreadID), Status: gen.MessageStatus(m.Status), Origin: map[string]any{},
		ClientId: m.ClientID, EditedAt: timePtr(m.EditedAt), DeletedAt: timePtr(m.DeletedAt), CreatedAt: m.CreatedAt.Time,
		Mentions: make([]gen.MemberRef, 0, len(v.Mentions)), Reactions: make([]gen.Reaction, 0, len(v.Reactions)),
		Attachments: make([]gen.Attachment, 0, len(v.Attachments)),
	}
	if len(m.Origin) > 0 {
		_ = json.Unmarshal(m.Origin, &out.Origin)
	}
	for _, mm := range v.Mentions {
		out.Mentions = append(out.Mentions, gen.MemberRef{Kind: gen.MemberRefKind(mm.Kind), Id: toUUID(mm.ID)})
	}
	for _, r := range v.Reactions {
		members := make([]gen.MemberRef, 0, len(r.Members))
		for _, mm := range r.Members {
			members = append(members, gen.MemberRef{Kind: gen.MemberRefKind(mm.Kind), Id: toUUID(mm.ID)})
		}
		out.Reactions = append(out.Reactions, gen.Reaction{Emoji: r.Emoji, Count: len(members), Members: members})
	}
	for _, a := range v.Attachments {
		out.Attachments = append(out.Attachments, toAttachment(a))
	}
	return out
}

func toCard(m db.Message) *gen.Card {
	if len(m.Card) == 0 {
		return nil
	}
	var c gen.Card
	if err := json.Unmarshal(m.Card, &c); err != nil {
		return nil
	}
	return &c
}
