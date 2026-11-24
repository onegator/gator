package conversations

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// Threads finds the thread under a message, opening it the first time somebody asks.
type Threads struct {
	pool   *pgxpool.Pool
	access *Access
}

// NewThreads builds the thread finder.
func NewThreads(pool *pgxpool.Pool, access *Access) *Threads {
	return &Threads{pool: pool, access: access}
}

// Of answers the thread under a message the caller may read. The first ask opens it, with the
// message's author in it so that replies reach them; a task card's thread is the task's own.
// Replies have no threads of their own: threads do not nest.
func (t *Threads) Of(ctx context.Context, p auth.Principal, message pgtype.UUID) (db.Conversation, error) {
	q := db.New(t.pool)
	msg, err := q.GetMessage(ctx, message)
	if err != nil {
		return db.Conversation{}, err
	}
	conv, rights, err := t.access.RightsByID(ctx, msg.ConversationID, p)
	if err != nil {
		return db.Conversation{}, err
	}
	if !rights.Read {
		return db.Conversation{}, ErrNotFound
	}
	if existing, err := q.GetThreadOfMessage(ctx, msg.ID); err == nil {
		return existing, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.Conversation{}, err
	}
	if conv.ParentMessageID.Valid || conv.Kind == KindTask {
		return db.Conversation{}, invalid("a reply has no thread of its own")
	}
	by, ok := authorOf(p)
	if !ok {
		by = System
	}
	return t.open(ctx, conv, msg, by)
}

// open creates the thread under msg, or answers the one a concurrent request just created.
func (t *Threads) open(ctx context.Context, conv db.Conversation, msg db.Message, by Author) (db.Conversation, error) {
	var thread db.Conversation
	err := store.InTx(ctx, t.pool, func(q *db.Queries) error {
		var err error
		thread, err = q.CreateConversation(ctx, db.CreateConversationParams{
			ProjectID: conv.ProjectID, Kind: KindThread, Visibility: conv.Visibility, ParentMessageID: msg.ID,
			CreatedByKind: by.Kind, CreatedBy: by.ID,
		})
		if err != nil {
			return err
		}
		if msg.AuthorKind == AuthorUser && msg.AuthorID.Valid {
			if _, err := q.AddConversationMember(ctx, db.AddConversationMemberParams{
				ConversationID: thread.ID, MemberKind: MemberUser, MemberID: msg.AuthorID, Role: RoleMember, Notify: NotifyMentions,
			}); err != nil {
				return err
			}
		}
		return emit(ctx, q, EventConversationCreated, thread.ID, map[string]any{
			"kind": thread.Kind, "parentMessageId": uuidString(msg.ID), "projectId": uuidString(conv.ProjectID),
		})
	})
	if isUniqueViolation(err) {
		return db.New(t.pool).GetThreadOfMessage(ctx, msg.ID)
	}
	return thread, err
}
