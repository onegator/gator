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

// Threads finds the thread under a message, opening it the first time somebody asks, and the
// thread every task has.
type Threads struct {
	pool     *pgxpool.Pool
	access   *Access
	channels taskChannels
}

// taskChannels answers where a project's new tasks show up. Directory is one.
type taskChannels interface {
	TaskChannel(ctx context.Context, project pgtype.UUID) (db.Conversation, error)
}

// NewThreads builds the thread finder.
func NewThreads(pool *pgxpool.Pool, access *Access, channels taskChannels) *Threads {
	return &Threads{pool: pool, access: access, channels: channels}
}

// ForTaskAs is ForTask for a caller: a task's thread that is there is answered to anyone who may
// read the task, and only someone who may write in the project opens a missing one (the
// consumer opens it for every new task in any case).
func (t *Threads) ForTaskAs(ctx context.Context, p auth.Principal, task db.Task) (db.Conversation, error) {
	conv, err := db.New(t.pool).GetTaskConversation(ctx, task.ID)
	if !errors.Is(err, pgx.ErrNoRows) {
		return conv, err
	}
	role, err := t.access.roles.ProjectRole(ctx, p, task.ProjectID)
	if err != nil {
		return db.Conversation{}, err
	}
	if role < auth.RoleMember {
		return db.Conversation{}, ErrNotFound
	}
	return t.ForTask(ctx, task)
}

// ForTask answers a task's thread, opening it when the task has none: the task's card goes into
// its project's task channel and the thread hangs under that card, so replying to the card is
// talking in the task. Safe to repeat and to race: there is one thread per task.
func (t *Threads) ForTask(ctx context.Context, task db.Task) (db.Conversation, error) {
	q := db.New(t.pool)
	if conv, err := q.GetTaskConversation(ctx, task.ID); !errors.Is(err, pgx.ErrNoRows) {
		return conv, err
	}
	var channel db.Conversation
	if !task.ClosedAt.Valid { // a task closed before it had a thread gets one quietly, without a card
		var err error
		if channel, err = t.channels.TaskChannel(ctx, task.ProjectID); err != nil {
			return db.Conversation{}, err
		}
	}
	var conv db.Conversation
	err := store.InTx(ctx, t.pool, func(q *db.Queries) error {
		var root pgtype.UUID
		if channel.ID.Valid {
			card, _, err := writeCard(ctx, q, channel, TaskCard(task))
			if err != nil {
				return err
			}
			root = card.ID
		}
		var err error
		if conv, err = q.CreateConversation(ctx, db.CreateConversationParams{
			ProjectID: task.ProjectID, Kind: KindTask, Visibility: VisibilityProject, ParentMessageID: root, TaskID: task.ID,
			CreatedByKind: AuthorSystem,
		}); err != nil {
			return err
		}
		return emit(ctx, q, EventConversationCreated, conv.ID, map[string]any{
			"kind": conv.Kind, "taskId": uuidString(task.ID), "parentMessageId": uuidString(root), "projectId": uuidString(task.ProjectID),
		})
	})
	if isUniqueViolation(err) { // opened a moment ago by another request
		return q.GetTaskConversation(ctx, task.ID)
	}
	return conv, err
}

// Of answers the thread under a message the caller may read. The first ask by someone who may
// write there opens it, with the message's author in it so that replies reach them; a task
// card's thread is the task's own. Replies have no threads of their own: threads do not nest.
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
	// Opening a thread is the first step of replying: someone who cannot write here (a viewer,
	// anyone in an archived conversation) only finds a thread that is there.
	if !rights.Write {
		return db.Conversation{}, ErrNotFound
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
