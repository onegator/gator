package conversations

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// EditWindow is how long a message stays its author's to change.
const EditWindow = 24 * time.Hour

const maxEmojiBytes = 64

// Edit changes the words of one's own message within EditWindow of posting it. Cards follow the
// object they show and are not edited by hand; who a message mentions stays as posted.
func (m *Messages) Edit(ctx context.Context, p auth.Principal, id pgtype.UUID, body string) (db.Message, error) {
	msg, rights, err := m.messageRights(ctx, p, id)
	if err != nil {
		return db.Message{}, err
	}
	author, _ := authorOf(p)
	switch {
	case msg.DeletedAt.Valid:
		return db.Message{}, conflict("the message was deleted")
	case msg.Kind != messageText:
		return db.Message{}, conflict("a card follows what it shows and is not edited by hand")
	case !sameAuthor(msg, author) || !rights.Write:
		return db.Message{}, ErrForbidden
	case m.now().Sub(msg.CreatedAt.Time) > EditWindow:
		return db.Message{}, conflict("a message can be edited for %s after it is posted", EditWindow)
	}
	d := Draft{Body: strings.TrimSpace(body)}
	if d.Body == "" {
		return db.Message{}, invalid("a message needs a body; delete it instead")
	}
	if err := d.validate(); err != nil {
		return db.Message{}, err
	}
	var out db.Message
	err = store.InTx(ctx, m.pool, func(q *db.Queries) error {
		var err error
		if out, err = q.EditMessage(ctx, db.EditMessageParams{ID: msg.ID, Body: d.Body}); err != nil {
			return err
		}
		return emit(ctx, q, EventMessageEdited, out.ConversationID, messagePayload(out, nil))
	})
	return out, err
}

// Delete takes a message's words away, leaving its place: its author may, and so may whoever
// moderates the conversation. Deleting it again changes nothing.
func (m *Messages) Delete(ctx context.Context, p auth.Principal, id pgtype.UUID) error {
	msg, rights, err := m.messageRights(ctx, p, id)
	if err != nil {
		return err
	}
	author, _ := authorOf(p)
	switch {
	case msg.DeletedAt.Valid:
		return nil
	case msg.Kind != messageText && !rights.Moderate:
		return conflict("a card follows what it shows; only a moderator takes one away")
	case !sameAuthor(msg, author) && !rights.Moderate:
		return ErrForbidden
	}
	return store.InTx(ctx, m.pool, func(q *db.Queries) error {
		out, err := q.SoftDeleteMessage(ctx, msg.ID)
		if err != nil {
			return ignoreGone(err) // deleted a moment ago
		}
		return emit(ctx, q, EventMessageDeleted, out.ConversationID, messagePayload(out, nil))
	})
}

// React adds (add) or takes back the caller's emoji on a message. Viewers react too. Saying the
// same thing twice changes nothing and tells nobody.
func (m *Messages) React(ctx context.Context, p auth.Principal, id pgtype.UUID, emoji string, add bool) error {
	msg, rights, err := m.messageRights(ctx, p, id)
	if err != nil {
		return err
	}
	me, ok := memberOf(p)
	if !ok || !rights.React {
		return ErrForbidden
	}
	if msg.DeletedAt.Valid {
		return conflict("the message was deleted")
	}
	if !validEmoji(emoji) {
		return invalid("an emoji is 1–%d bytes without spaces", maxEmojiBytes)
	}
	return store.InTx(ctx, m.pool, func(q *db.Queries) error {
		var n int64
		var err error
		if add {
			n, err = q.AddReaction(ctx, db.AddReactionParams{MessageID: msg.ID, MemberKind: me.Kind, MemberID: me.ID, Emoji: emoji})
		} else {
			n, err = q.RemoveReaction(ctx, db.RemoveReactionParams{MessageID: msg.ID, MemberKind: me.Kind, MemberID: me.ID, Emoji: emoji})
		}
		if err != nil || n == 0 {
			return err
		}
		return emit(ctx, q, EventReactionChanged, msg.ConversationID, map[string]any{
			eventMessageID: uuidString(msg.ID), "emoji": emoji, "member": memberPayload(me), "added": add,
		})
	})
}

// messageRights loads a message and the caller's rights where it is; a message the caller may
// not read does not exist for them.
func (m *Messages) messageRights(ctx context.Context, p auth.Principal, id pgtype.UUID) (db.Message, Rights, error) {
	msg, err := db.New(m.pool).GetMessage(ctx, id)
	if err != nil {
		return db.Message{}, Rights{}, err
	}
	_, rights, err := m.access.RightsByID(ctx, msg.ConversationID, p)
	if err != nil {
		return db.Message{}, Rights{}, err
	}
	if !rights.Read {
		return db.Message{}, Rights{}, ErrNotFound
	}
	return msg, rights, nil
}

func sameAuthor(msg db.Message, a Author) bool {
	return a.ID.Valid && msg.AuthorKind == a.Kind && msg.AuthorID == a.ID
}

// validEmoji accepts a short token without spaces or control characters: a Unicode emoji or a
// :shortcode:.
func validEmoji(e string) bool {
	if e == "" || len(e) > maxEmojiBytes {
		return false
	}
	for _, r := range e {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// describeReactions adds who reacted with what to a page of messages.
func describeReactions(ctx context.Context, q *db.Queries, ids []pgtype.UUID, views []View, at map[[16]byte]int) error {
	rows, err := q.ListReactionsOfMessages(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range rows {
		v := &views[at[r.MessageID.Bytes]]
		if n := len(v.Reactions); n == 0 || v.Reactions[n-1].Emoji != r.Emoji {
			v.Reactions = append(v.Reactions, Reaction{Emoji: r.Emoji})
		}
		last := &v.Reactions[len(v.Reactions)-1]
		last.Members = append(last.Members, Member{Kind: r.MemberKind, ID: r.MemberID})
	}
	return nil
}
