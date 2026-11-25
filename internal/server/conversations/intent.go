package conversations

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// What a person meant a message for while an agent was answering (messages.input_intent).
const (
	IntentTurnStart       = "turn_start"
	IntentSteer           = "steer"
	IntentQueued          = "queued"
	IntentPromotedToSteer = "promoted_to_steer"
)

// EventMessageDequeued says a queued message leaves the queue: it asks now, as a message just
// posted would (a turn starts, or, promoted, the running turn is steered). Its payload names the
// agents it was queued for ("agents"): those answering when it was written, whom it asks besides
// whom it mentions.
const EventMessageDequeued = "message.dequeued"

// intentOf is what a person's message is for. While a turn runs in the conversation it steers the
// turn unless the person queues it — and a command (a line that starts with /) always waits for
// the turn's end. With no turn running it is an ordinary message and says nothing.
// A queued message keeps the agents it was queued for: those answering when it was written.
func intentOf(ctx context.Context, q *db.Queries, conv db.Conversation, author Author, d Draft) (*string, []pgtype.UUID, error) {
	if author.Kind != AuthorUser {
		return nil, nil, nil
	}
	turns, err := q.ListActiveTurnsIn(ctx, conv.ID)
	if err != nil || len(turns) == 0 {
		return nil, nil, err
	}
	intent := IntentSteer
	if d.Intent != IntentQueued && !strings.HasPrefix(strings.TrimSpace(d.Body), "/") {
		return &intent, nil, nil
	}
	intent = IntentQueued
	var agents []pgtype.UUID
	for _, t := range turns {
		if !slices.Contains(agents, t.AgentID) {
			agents = append(agents, t.AgentID)
		}
	}
	return &intent, agents, nil
}

// Queue is a conversation's queue of messages for after the agent's turn.
type Queue struct {
	pool   *pgxpool.Pool
	access *Access
}

// NewQueue builds it.
func NewQueue(pool *pgxpool.Pool, access *Access) *Queue { return &Queue{pool: pool, access: access} }

// List answers a conversation's queue for whoever may read it.
func (qu *Queue) List(ctx context.Context, p auth.Principal, conversation pgtype.UUID) ([]View, error) {
	conv, rights, err := qu.access.RightsByID(ctx, conversation, p)
	if err != nil {
		return nil, err
	}
	if !rights.Read {
		return nil, ErrNotFound
	}
	q := db.New(qu.pool)
	rows, err := q.ListQueuedMessages(ctx, conv.ID)
	if err != nil {
		return nil, err
	}
	return describe(ctx, q, rows)
}

// SendNow is "send now as a steer": the author takes their message out of the queue and it asks at
// once — it steers the running turn, or starts one when none runs any more.
func (qu *Queue) SendNow(ctx context.Context, p auth.Principal, id pgtype.UUID) (db.Message, error) {
	q := db.New(qu.pool)
	msg, err := q.GetMessage(ctx, id)
	if err != nil {
		return db.Message{}, found(err)
	}
	_, rights, err := qu.access.RightsByID(ctx, msg.ConversationID, p)
	if err != nil {
		return db.Message{}, err
	}
	if !rights.Read {
		return db.Message{}, ErrNotFound
	}
	if author, ok := authorOf(p); !ok || !rights.Write || author.Kind != msg.AuthorKind || author.ID != msg.AuthorID {
		return db.Message{}, ErrForbidden
	}
	if len(msg.QueuedFor) == 0 {
		mentions, err := q.ListMentionsOfMessages(ctx, []pgtype.UUID{msg.ID})
		if err != nil {
			return db.Message{}, err
		}
		if !slices.ContainsFunc(mentions, func(m db.MessageMention) bool { return m.MemberKind == MemberAgent }) {
			return db.Message{}, conflict("this message asks no agent; it stays queued")
		}
	}
	var out db.Message
	err = store.InTx(ctx, qu.pool, func(q *db.Queries) error {
		var err error
		intent := IntentPromotedToSteer
		if out, err = q.SetMessageIntent(ctx, db.SetMessageIntentParams{ID: msg.ID, Intent: &intent}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return conflict("this message is no longer queued")
			}
			return err
		}
		// It was queued for the agents answering when it was written: it steers those still
		// answering, and asks those whose turn ended meanwhile.
		return emit(ctx, q, EventMessageDequeued, out.ConversationID, messagePayload(out, map[string]any{"intent": intent,
			"agents": uuidStrings(out.QueuedFor)}))
	})
	return out, err
}

// NextQueued takes the first message of a conversation's queue out of it to start the next turn,
// once no turn runs there; it asks the agents it was queued for. It answers whether it took one.
func NextQueued(ctx context.Context, pool *pgxpool.Pool, conversation pgtype.UUID) (bool, error) {
	took := false
	err := store.InTx(ctx, pool, func(q *db.Queries) error {
		if err := q.LockConversation(ctx, conversation); err != nil {
			return err
		}
		active, err := q.CountActiveTurnsIn(ctx, conversation)
		if err != nil || active > 0 {
			return err
		}
		next, err := q.NextQueuedMessage(ctx, conversation)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		intent := IntentTurnStart
		out, err := q.SetMessageIntent(ctx, db.SetMessageIntentParams{ID: next.ID, Intent: &intent})
		if err != nil {
			return err
		}
		took = true
		return emit(ctx, q, EventMessageDequeued, out.ConversationID, messagePayload(out, map[string]any{"intent": intent,
			"agents": uuidStrings(out.QueuedFor)}))
	})
	return took, err
}
