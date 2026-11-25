package conversations

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// Limits on what one message carries and on a page of them.
const (
	maxBodyRunes = 40000
	maxClientID  = 200
	maxMentions  = 50
	defaultPage  = 50
	maxPage      = 200
)

const (
	messageText    = "text"
	messageCard    = "card"
	messageFinal   = "final"
	emptyOrigin    = "{}"
	eventMessageID = "messageId"
)

// Messages posts and reads the messages of a conversation.
type Messages struct {
	pool   *pgxpool.Pool
	access *Access
	// attach hands a message the files uploaded for it, in the posting transaction. Nil: no
	// attachments (see Attachments).
	attach attacher
	now    func() time.Time
}

// attacher puts uploaded files on a message being posted.
type attacher interface {
	attach(ctx context.Context, q *db.Queries, msg db.Message, by Author, ids []pgtype.UUID) error
}

// NewMessages builds the message service.
func NewMessages(pool *pgxpool.Pool, access *Access) *Messages {
	return &Messages{pool: pool, access: access, now: time.Now}
}

// Draft is a message about to be posted.
type Draft struct {
	Body string
	// ClientID is the sender's idempotency key: the same sender posting it again in the same
	// conversation answers the message it made. Another sender's key is theirs.
	ClientID string
	// Mentions are the people the message names. Someone who cannot read the conversation is
	// left out rather than told about it.
	Mentions    []Member
	Attachments []pgtype.UUID
	// Intent is what a person means a message for while an agent answers in the conversation:
	// IntentSteer (the default: a correction of the turn) or IntentQueued (the next thing to do
	// once it ends). With no turn running there is nothing to steer or wait for, and it is ignored.
	Intent string
	// Within runs in the post's own transaction once the message is written, before anyone hears
	// of it: what belongs to the message (the review comments it carries) is there with it, or the
	// message is not. An error from it fails the post.
	Within func(ctx context.Context, q *db.Queries, msg db.Message) error
}

// Posted is the outcome of a post.
type Posted struct {
	Message db.Message
	// Replayed is a post whose client id had already been posted: nothing new was written.
	Replayed bool
}

// View is a message with what is said about it: who it mentions, its thread, its reactions and
// its files.
type View struct {
	Message     db.Message
	Mentions    []Member
	ThreadID    pgtype.UUID
	Reactions   []Reaction
	Attachments []db.MessageAttachment
	// Turn is the agent's turn this message is the answer of, with what it cost: what a client
	// shows under the answer. Nil for anything else.
	Turn *db.ListTurnsOfRepliesRow
}

// Reaction is one emoji on a message and who added it.
type Reaction struct {
	Emoji   string
	Members []Member
}

// Page asks for messages by seq: the newest page, the page before a seq, or the messages after
// one (AfterSeq 0 is from the first message).
type Page struct {
	BeforeSeq int64
	AfterSeq  *int64
	Limit     int
}

// PageResult is one page in seq order.
type PageResult struct {
	Messages []View
	// HasMore says there are more messages beyond this page in the direction asked.
	HasMore bool
}

// replay carries the message a repeated client id already made.
type replay struct{ msg db.Message }

func (replay) Error() string { return "already posted" }

// Post writes a message. Its seq is taken under the conversation's row lock, so concurrent
// posts are numbered in commit order without gaps, and a repeated client id is found under the
// same lock: the second request answers the first one's message, and its seq is never used.
func (m *Messages) Post(ctx context.Context, p auth.Principal, conversation pgtype.UUID, d Draft) (Posted, error) {
	conv, rights, err := m.access.RightsByID(ctx, conversation, p)
	if err != nil {
		return Posted{}, err
	}
	if !rights.Read {
		return Posted{}, ErrNotFound
	}
	author, ok := authorOf(p)
	if !rights.Write || !ok {
		return Posted{}, ErrForbidden
	}
	d.Body = strings.TrimSpace(d.Body)
	if err := d.validate(); err != nil {
		return Posted{}, err
	}
	mentioned, err := m.readers(ctx, conv, d.Mentions)
	if err != nil {
		return Posted{}, err
	}
	var msg db.Message
	err = store.InTxRetry(ctx, m.pool, func(q *db.Queries) error {
		seq, err := q.NextMessageSeq(ctx, conv.ID)
		if err != nil {
			return err
		}
		if d.ClientID != "" {
			prior, err := q.GetMessageByClientID(ctx, db.GetMessageByClientIDParams{
				ConversationID: conv.ID, AuthorKind: author.Kind, AuthorID: author.ID, ClientID: &d.ClientID,
			})
			if err == nil {
				return replay{prior}
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		intent, err := intentOf(ctx, q, conv, author, d)
		if err != nil {
			return err
		}
		if msg, err = q.InsertMessage(ctx, db.InsertMessageParams{
			ConversationID: conv.ID, Seq: seq, AuthorKind: author.Kind, AuthorID: author.ID, Kind: messageText,
			Body: d.Body, ClientID: optional(d.ClientID), Origin: []byte(emptyOrigin), Status: messageFinal, InputIntent: intent,
		}); err != nil {
			return err
		}
		for _, mm := range mentioned {
			if err := q.AddMention(ctx, db.AddMentionParams{MessageID: msg.ID, MemberKind: mm.Kind, MemberID: mm.ID}); err != nil {
				return err
			}
		}
		if len(d.Attachments) > 0 {
			if m.attach == nil {
				return invalid("this server keeps no files")
			}
			if err := m.attach.attach(ctx, q, msg, author, d.Attachments); err != nil {
				return err
			}
		}
		if d.Within != nil {
			if err := d.Within(ctx, q, msg); err != nil {
				return err
			}
		}
		return afterPost(ctx, q, conv, msg, author, mentioned)
	})
	var again replay
	if errors.As(err, &again) {
		return Posted{Message: again.msg, Replayed: true}, nil
	}
	return Posted{Message: msg}, err
}

func (d Draft) validate() error {
	switch {
	case d.Intent != "" && d.Intent != IntentSteer && d.Intent != IntentQueued:
		return invalid("intent is steer or queued")
	case d.Body == "" && len(d.Attachments) == 0:
		return invalid("a message needs a body or a file")
	case utf8.RuneCountInString(d.Body) > maxBodyRunes:
		return invalid("a message is at most %d characters", maxBodyRunes)
	case len(d.ClientID) > maxClientID:
		return invalid("clientId is at most %d characters", maxClientID)
	case len(d.Mentions) > maxMentions:
		return invalid("a message mentions at most %d members", maxMentions)
	}
	return nil
}

// readers keeps the mentioned members who may be asked here, once each: people who may read the
// conversation, and agents of its project. Mentioning an agent is asking it.
func (m *Messages) readers(ctx context.Context, conv db.Conversation, mentions []Member) ([]Member, error) {
	seen := map[[16]byte]bool{}
	var out []Member
	for _, mm := range mentions {
		if mm.Kind != MemberUser && mm.Kind != MemberAgent {
			return nil, invalid("a mention names a person or an agent")
		}
		if !mm.ID.Valid || seen[mm.ID.Bytes] {
			continue
		}
		seen[mm.ID.Bytes] = true
		if mm.Kind == MemberAgent {
			root, err := m.access.root(ctx, conv)
			if err != nil {
				return nil, err
			}
			ok, err := agentVisible(ctx, db.New(m.pool), root.ProjectID, mm.ID)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, invalid("no such agent in this conversation's project")
			}
			out = append(out, mm)
			continue
		}
		ok, err := m.access.UserCanRead(ctx, conv, mm.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, mm)
		}
	}
	return out, nil
}

// afterPost does what every new message brings with it, in its transaction: a person who posts
// is in the conversation from then on and has read what they wrote, a reply moves its root's
// count, and the event goes out.
func afterPost(ctx context.Context, q *db.Queries, conv db.Conversation, msg db.Message, author Author, mentioned []Member) error {
	if author.Kind == AuthorUser {
		if err := q.JoinAsAuthor(ctx, db.JoinAsAuthorParams{ConversationID: conv.ID, UserID: author.ID, Seq: msg.Seq}); err != nil {
			return err
		}
	}
	if conv.ParentMessageID.Valid {
		root, err := q.IncrementReplyCount(ctx, conv.ParentMessageID)
		if err != nil {
			return err
		}
		if err := emit(ctx, q, EventMessageReplied, root.ConversationID, map[string]any{
			eventMessageID: uuidString(root.ID), "replyCount": root.ReplyCount, "threadId": uuidString(conv.ID),
		}); err != nil {
			return err
		}
	}
	people, agents := []pgtype.UUID{}, []pgtype.UUID{}
	for _, mm := range mentioned {
		if mm.Kind == MemberAgent {
			agents = append(agents, mm.ID)
		} else {
			people = append(people, mm.ID)
		}
	}
	extra := map[string]any{"mentions": uuidStrings(people)}
	if len(agents) > 0 {
		extra["agents"] = uuidStrings(agents)
	}
	return emit(ctx, q, EventMessagePosted, conv.ID, messagePayload(msg, extra), people...)
}

// postSystemText writes a line Gator says by itself into conv, in the caller's transaction.
func postSystemText(ctx context.Context, q *db.Queries, conv db.Conversation, body string) (db.Message, error) {
	seq, err := q.NextMessageSeq(ctx, conv.ID)
	if err != nil {
		return db.Message{}, err
	}
	msg, err := q.InsertMessage(ctx, db.InsertMessageParams{
		ConversationID: conv.ID, Seq: seq, AuthorKind: AuthorSystem, Kind: messageText, Body: body,
		Origin: []byte(emptyOrigin), Status: messageFinal,
	})
	if err != nil {
		return db.Message{}, err
	}
	return msg, afterPost(ctx, q, conv, msg, System, nil)
}

// messagePayload is what an event says about a message: never its body.
func messagePayload(msg db.Message, extra map[string]any) map[string]any {
	out := map[string]any{
		eventMessageID: uuidString(msg.ID), "seq": msg.Seq, "kind": msg.Kind, "authorKind": msg.AuthorKind,
	}
	if msg.AuthorID.Valid {
		out["authorId"] = uuidString(msg.AuthorID)
	}
	if msg.CardType != nil {
		out["cardType"] = *msg.CardType
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// List answers one page of a conversation's messages, in seq order.
func (m *Messages) List(ctx context.Context, p auth.Principal, conversation pgtype.UUID, pg Page) (PageResult, error) {
	conv, rights, err := m.access.RightsByID(ctx, conversation, p)
	if err != nil {
		return PageResult{}, err
	}
	if !rights.Read {
		return PageResult{}, ErrNotFound
	}
	limit := pg.Limit
	if limit <= 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)
	q := db.New(m.pool)
	if pg.AfterSeq != nil {
		rows, err := q.ListMessagesAfter(ctx, db.ListMessagesAfterParams{ConversationID: conv.ID, AfterSeq: *pg.AfterSeq, MaxRows: int32(limit + 1)})
		if err != nil {
			return PageResult{}, err
		}
		more := len(rows) > limit
		if more {
			rows = rows[:limit]
		}
		return m.page(ctx, q, rows, more)
	}
	before := pg.BeforeSeq
	if before <= 0 {
		before = math.MaxInt64
	}
	rows, err := q.ListMessagesBefore(ctx, db.ListMessagesBeforeParams{ConversationID: conv.ID, BeforeSeq: before, MaxRows: int32(limit + 1)})
	if err != nil {
		return PageResult{}, err
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit] // newest first: the extra one is the oldest
	}
	reverse(rows)
	return m.page(ctx, q, rows, more)
}

func (m *Messages) page(ctx context.Context, q *db.Queries, rows []db.Message, more bool) (PageResult, error) {
	views, err := describe(ctx, q, rows)
	return PageResult{Messages: views, HasMore: more}, err
}

// Get answers one message the caller may read.
func (m *Messages) Get(ctx context.Context, p auth.Principal, id pgtype.UUID) (View, error) {
	q := db.New(m.pool)
	msg, err := q.GetMessage(ctx, id)
	if err != nil {
		return View{}, found(err)
	}
	_, rights, err := m.access.RightsByID(ctx, msg.ConversationID, p)
	if err != nil {
		return View{}, err
	}
	if !rights.Read {
		return View{}, ErrNotFound
	}
	views, err := describe(ctx, q, []db.Message{msg})
	if err != nil {
		return View{}, err
	}
	return views[0], nil
}

// MarkRead moves the caller's read marker up to seq; reading a conversation is being in it.
func (m *Messages) MarkRead(ctx context.Context, p auth.Principal, conversation pgtype.UUID, seq int64) (db.ConversationMember, error) {
	conv, rights, err := m.access.RightsByID(ctx, conversation, p)
	if err != nil {
		return db.ConversationMember{}, err
	}
	if !rights.Read {
		return db.ConversationMember{}, ErrNotFound
	}
	me, ok := memberOf(p)
	if !ok {
		return db.ConversationMember{}, ErrForbidden
	}
	if seq < 0 {
		return db.ConversationMember{}, invalid("seq is not negative")
	}
	var out db.ConversationMember
	err = store.InTx(ctx, m.pool, func(q *db.Queries) error {
		before := int64(-1) // not in it yet: joining is news
		if prior, err := q.GetConversationMember(ctx, db.GetConversationMemberParams{ConversationID: conv.ID, MemberKind: me.Kind, MemberID: me.ID}); err == nil {
			before = prior.LastReadSeq
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var err error
		if out, err = q.MarkConversationRead(ctx, db.MarkConversationReadParams{UserID: me.ID, Seq: seq, ConversationID: conv.ID}); err != nil {
			return err
		}
		if out.LastReadSeq == before {
			return nil
		}
		// Only the reader's other devices care.
		return emitPersonal(ctx, q, EventConversationRead, conv.ID, map[string]any{"lastReadSeq": out.LastReadSeq}, me.ID)
	})
	return out, err
}

// describe adds to messages what is said about them, in a few queries for the whole page.
func describe(ctx context.Context, q *db.Queries, rows []db.Message) ([]View, error) {
	views := make([]View, len(rows))
	ids := make([]pgtype.UUID, len(rows))
	at := map[[16]byte]int{}
	for i, r := range rows {
		views[i].Message = blankIfDeleted(r)
		ids[i] = r.ID
		at[r.ID.Bytes] = i
	}
	if len(rows) == 0 {
		return views, nil
	}
	mentions, err := q.ListMentionsOfMessages(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, mm := range mentions {
		v := &views[at[mm.MessageID.Bytes]]
		v.Mentions = append(v.Mentions, Member{Kind: mm.MemberKind, ID: mm.MemberID})
	}
	threads, err := q.ListThreadsOfMessages(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, t := range threads {
		views[at[t.ParentMessageID.Bytes]].ThreadID = t.ID
	}
	if err := describeReactions(ctx, q, ids, views, at); err != nil {
		return nil, err
	}
	if err := describeAttachments(ctx, q, ids, views, at); err != nil {
		return nil, err
	}
	turns, err := q.ListTurnsOfReplies(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, t := range turns {
		views[at[t.ReplyMessageID.Bytes]].Turn = &t
	}
	return views, nil
}

// blankIfDeleted keeps what a deleted message was from its readers: only that it was there.
func blankIfDeleted(m db.Message) db.Message {
	if m.DeletedAt.Valid {
		m.Body, m.Card = "", nil
	}
	return m
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
