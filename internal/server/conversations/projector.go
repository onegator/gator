package conversations

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// CardSource turns domain events into the cards that show them. Each kind of card is one source;
// a new kind is a new source registered with the consumer, and nothing else changes.
type CardSource interface {
	// Events are the outbox events the source reads.
	Events() []string
	// Cards answers the cards an event leaves, in their current state. Answering a card that is
	// already there as it is changes nothing, so a source need not know what it said before.
	Cards(ctx context.Context, e db.EventsAfterRow) ([]PlacedCard, error)
}

// PlacedCard is a card and the task it belongs to.
type PlacedCard struct {
	Task db.Task
	// Root is the task's own card, which sits where the task's thread hangs (in the task channel)
	// rather than inside it. Every other card goes into the task's thread.
	Root bool
	Card Card
}

// CardIndex answers the cards of one type already in a task's thread, for a source that has to
// take back what it said (a decision that no longer waits).
type CardIndex interface {
	TaskCards(ctx context.Context, task db.Task, cardType string) ([]db.Message, error)
}

// taskThreads answers a task's thread, opening it when it has none. Threads is one.
type taskThreads interface {
	ForTask(ctx context.Context, task db.Task) (db.Conversation, error)
}

// Projector writes the cards sources answer into conversations.
type Projector struct {
	pool    *pgxpool.Pool
	threads taskThreads
}

// NewProjector builds the projector.
func NewProjector(pool *pgxpool.Pool, threads taskThreads) *Projector {
	return &Projector{pool: pool, threads: threads}
}

// Handler is the consumer handler for one source.
func (p *Projector) Handler(src CardSource) Handler {
	return HandlerFunc(func(ctx context.Context, e db.EventsAfterRow) error {
		cards, err := src.Cards(ctx, e)
		if err != nil {
			return err
		}
		for _, c := range cards {
			if err := p.Place(ctx, c); err != nil {
				return err
			}
		}
		return nil
	})
}

// Place writes one card where it belongs.
func (p *Projector) Place(ctx context.Context, c PlacedCard) error {
	thread, err := p.threads.ForTask(ctx, c.Task)
	if err != nil {
		return err
	}
	target := thread
	if c.Root {
		if target, err = p.rootHome(ctx, thread, c.Card.Key); err != nil {
			return err
		}
	}
	return store.InTx(ctx, p.pool, func(q *db.Queries) error {
		_, _, err := writeCard(ctx, q, target, c.Card)
		return err
	})
}

// rootHome is where a task's own card lives: the conversation of the card the thread hangs
// under. A thread that hangs under somebody's message (a message made into a task) or under
// nothing (a task older than conversations) has the card inside it instead.
func (p *Projector) rootHome(ctx context.Context, thread db.Conversation, key string) (db.Conversation, error) {
	if !thread.ParentMessageID.Valid {
		return thread, nil
	}
	q := db.New(p.pool)
	root, err := q.GetMessage(ctx, thread.ParentMessageID)
	if err != nil {
		return db.Conversation{}, err
	}
	if root.CardKey == nil || *root.CardKey != key {
		return thread, nil
	}
	return q.GetConversation(ctx, root.ConversationID)
}

// TaskCards implements CardIndex.
func (p *Projector) TaskCards(ctx context.Context, task db.Task, cardType string) ([]db.Message, error) {
	thread, err := p.threads.ForTask(ctx, task)
	if err != nil {
		return nil, err
	}
	return db.New(p.pool).ListCardsOfType(ctx, db.ListCardsOfTypeParams{ConversationID: thread.ID, CardType: &cardType})
}

// taskReader is what card sources read about tasks. *db.Queries is one.
type taskReader interface {
	GetTask(ctx context.Context, id pgtype.UUID) (db.Task, error)
}

// ignoreGone treats an object deleted since its event as nothing to show.
func ignoreGone(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// taskOf loads the task an event is about; a task deleted since leaves no card.
func taskOf(ctx context.Context, tasks taskReader, id pgtype.UUID) (db.Task, bool, error) {
	t, err := tasks.GetTask(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Task{}, false, nil
	}
	return t, err == nil, err
}
