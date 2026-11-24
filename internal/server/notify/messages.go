package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// ChatAccess answers whether a person may read a conversation. conversations.Access is one.
type ChatAccess interface {
	UserCanRead(ctx context.Context, conv db.Conversation, user pgtype.UUID) (bool, error)
}

// WithChat has the notifier tell people about messages that concern them: a mention, a DM, a
// reply in a thread they wrote in, and everything in a conversation they follow with "all".
// Without it, messages notify nobody.
func (n *Notifier) WithChat(access ChatAccess) *Notifier {
	n.chat = access
	return n
}

// chatReason is why a person hears about a message; the first that applies wins.
type chatReason int

const (
	reasonMention chatReason = iota + 1
	reasonDM
	reasonThread
	reasonAll
)

// chatFacts are what the rule looks at for one message.
type chatFacts struct {
	author pgtype.UUID
	dm     bool
	// mentioned are the people the message names.
	mentioned []pgtype.UUID
	// thread are the people who wrote the root of the thread the message is a reply in, or a
	// reply in it; empty for a message that is no reply.
	thread []pgtype.UUID
	// members are the people in the conversation, with how they want to hear about it.
	members map[[16]byte]string
}

// chatRecipients is the rule: who hears about a message, and why. Nobody hears about their own
// message, and "none" silences a conversation, mentions included.
func chatRecipients(f chatFacts) map[[16]byte]chatReason {
	out := map[[16]byte]chatReason{}
	add := func(id pgtype.UUID, why chatReason) {
		if !id.Valid || id == f.author || f.members[id.Bytes] == "none" {
			return
		}
		if prior, ok := out[id.Bytes]; !ok || why < prior {
			out[id.Bytes] = why
		}
	}
	for _, id := range f.mentioned {
		add(id, reasonMention)
	}
	for member, setting := range f.members {
		id := pgtype.UUID{Bytes: member, Valid: true}
		switch {
		case f.dm:
			add(id, reasonDM)
		case setting == "all":
			add(id, reasonAll)
		}
	}
	for _, id := range f.thread {
		add(id, reasonThread)
	}
	return out
}

// handleMessage pushes a new message to the people it concerns, who may still read it.
func (n *Notifier) handleMessage(ctx context.Context, row db.EventsAfterRow) error {
	if n.chat == nil || row.Type != "message.posted" {
		return nil
	}
	var payload struct {
		MessageID string `json:"messageId"`
	}
	var id pgtype.UUID
	if json.Unmarshal(row.Payload, &payload) != nil || id.Scan(payload.MessageID) != nil {
		return nil
	}
	q := db.New(n.pool)
	msg, err := q.GetMessage(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Cards are work, and the task's own notifications already tell people about it.
	if msg.Kind != "text" || msg.DeletedAt.Valid {
		return nil
	}
	conv, err := q.GetConversation(ctx, msg.ConversationID)
	if err != nil {
		return err
	}
	facts, err := n.chatFacts(ctx, q, conv, msg)
	if err != nil {
		return err
	}
	where, err := n.where(ctx, q, conv)
	if err != nil {
		return err
	}
	author := n.authorName(ctx, q, msg)
	byReason := map[chatReason][]pgtype.UUID{}
	for user, why := range chatRecipients(facts) {
		uid := pgtype.UUID{Bytes: user, Valid: true}
		ok, err := n.chat.UserCanRead(ctx, conv, uid)
		if err != nil {
			return err
		}
		if ok {
			byReason[why] = append(byReason[why], uid)
		}
	}
	for why, users := range byReason {
		if err := n.deliver(ctx, users, chatMessage(why, author, where, conv, msg)); err != nil {
			return err
		}
	}
	return nil
}

func (n *Notifier) chatFacts(ctx context.Context, q *db.Queries, conv db.Conversation, msg db.Message) (chatFacts, error) {
	f := chatFacts{author: msg.AuthorID, dm: conv.Kind == "dm", members: map[[16]byte]string{}}
	if msg.AuthorKind != "user" {
		f.author = pgtype.UUID{}
	}
	members, err := q.ListConversationMembers(ctx, conv.ID)
	if err != nil {
		return f, err
	}
	for _, m := range members {
		if m.MemberKind == "user" {
			f.members[m.MemberID.Bytes] = m.Notify
		}
	}
	mentions, err := q.ListMentionsOfMessages(ctx, []pgtype.UUID{msg.ID})
	if err != nil {
		return f, err
	}
	for _, m := range mentions {
		if m.MemberKind == "user" {
			f.mentioned = append(f.mentioned, m.MemberID)
		}
	}
	if conv.ParentMessageID.Valid {
		if f.thread, err = q.ListThreadParticipants(ctx, db.ListThreadParticipantsParams{ConversationID: conv.ID, RootID: conv.ParentMessageID}); err != nil {
			return f, err
		}
	}
	return f, nil
}

// where names the conversation for a person: #channel, a DM, a thread in #channel, a task.
func (n *Notifier) where(ctx context.Context, q *db.Queries, conv db.Conversation) (string, error) {
	switch {
	case conv.Kind == "task" && conv.TaskID.Valid:
		t, err := q.GetTask(ctx, conv.TaskID)
		if err != nil {
			return "", err
		}
		return "“" + t.Title + "”", nil
	case conv.ParentMessageID.Valid:
		root, err := q.GetAccessRoot(ctx, conv.ID)
		if err != nil {
			return "", err
		}
		if root.Name != nil {
			return "a thread in #" + *root.Name, nil
		}
		return "a thread", nil
	case conv.Name != nil:
		return "#" + *conv.Name, nil
	}
	return "a direct message", nil
}

func (n *Notifier) authorName(ctx context.Context, q *db.Queries, msg db.Message) string {
	if msg.AuthorKind == "user" && msg.AuthorID.Valid {
		if u, err := q.GetUser(ctx, msg.AuthorID); err == nil && u.Name != "" {
			return u.Name
		}
		return "Someone"
	}
	return "Gator"
}

// chatMessage words one push.
func chatMessage(why chatReason, author, where string, conv db.Conversation, msg db.Message) Message {
	title := author + " in " + where
	switch why {
	case reasonMention:
		title = author + " mentioned you in " + where
	case reasonThread:
		title = author + " replied in " + where
	case reasonDM:
		if where == "a direct message" {
			title = author
		}
	}
	return Message{
		Title: title,
		Body:  preview(msg.Body, 180),
		Link:  fmt.Sprintf("gator://conversations/%s?seq=%d", uuidString(conv.ID), msg.Seq),
	}
}

func preview(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
