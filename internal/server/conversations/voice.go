package conversations

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// Statuses of a message an agent is still writing.
const (
	StatusStreaming = "streaming"
	StatusFinal     = messageFinal
	StatusFailed    = "failed"
)

// Voice is how an agent and Gator speak in a conversation on their own: the answer an agent
// writes while it works, the lines it adds, and what Gator says about a turn. People post
// through Messages, with their rights checked; whoever holds a Voice has decided already that
// this may be said (the agents package, for a turn it started).
type Voice struct {
	pool *pgxpool.Pool
}

// NewVoice builds it.
func NewVoice(pool *pgxpool.Pool) *Voice { return &Voice{pool: pool} }

// Speech is one message an agent says.
type Speech struct {
	Agent pgtype.UUID
	// Turn is the turn it is said in, written in the message's origin so that what it starts
	// (a mention of another agent) belongs to the same chain.
	Turn pgtype.UUID
	Body string
	// Status is StatusStreaming for an answer still being written, else final.
	Status string
	// Mentions are agents (and people) it asks. Nobody is mentioned in a streaming answer.
	Mentions []Member
}

// Origin is what an agent's message carries about where it was said.
type Origin struct {
	Turn string `json:"turn,omitempty"`
}

// OriginOf reads a message's origin.
func OriginOf(m db.Message) Origin {
	var o Origin
	_ = json.Unmarshal(m.Origin, &o)
	return o
}

// Say posts what an agent says into conv. The agent is in the conversation from then on.
func (v *Voice) Say(ctx context.Context, conv db.Conversation, s Speech) (db.Message, error) {
	if s.Status == "" {
		s.Status = StatusFinal
	}
	origin, _ := json.Marshal(Origin{Turn: uuidString(s.Turn)})
	var msg db.Message
	err := store.InTx(ctx, v.pool, func(q *db.Queries) error {
		seq, err := q.NextMessageSeq(ctx, conv.ID)
		if err != nil {
			return err
		}
		if msg, err = q.InsertMessage(ctx, db.InsertMessageParams{
			ConversationID: conv.ID, Seq: seq, AuthorKind: AuthorAgent, AuthorID: s.Agent, Kind: messageText,
			Body: strings.TrimSpace(s.Body), Origin: origin, Status: s.Status,
		}); err != nil {
			return err
		}
		for _, mm := range s.Mentions {
			if err := q.AddMention(ctx, db.AddMentionParams{MessageID: msg.ID, MemberKind: mm.Kind, MemberID: mm.ID}); err != nil {
				return err
			}
		}
		if _, err := joinAgent(ctx, q, conv.ID, s.Agent); err != nil {
			return err
		}
		return afterPost(ctx, q, conv, msg, Author{Kind: AuthorAgent, ID: s.Agent}, s.Mentions)
	})
	return msg, err
}

// Rewrite changes what a message an agent is writing says, and whether it is still being
// written. Saying the same again changes nothing and tells nobody; anything else is
// message.edited. A message somebody deleted stays deleted.
func (v *Voice) Rewrite(ctx context.Context, message pgtype.UUID, body, status string) (db.Message, bool, error) {
	var out db.Message
	changed := false
	err := store.InTx(ctx, v.pool, func(q *db.Queries) error {
		msg, err := q.GetMessageForUpdate(ctx, message)
		if err != nil {
			return err
		}
		out = msg
		if msg.DeletedAt.Valid || (msg.Body == body && msg.Status == status) {
			return nil
		}
		if out, err = q.RewriteMessage(ctx, db.RewriteMessageParams{ID: message, Body: body, Status: status}); err != nil {
			return err
		}
		changed = true
		return emit(ctx, q, EventMessageEdited, msg.ConversationID, messagePayload(out, map[string]any{"status": status}))
	})
	return out, changed, err
}

// Announce is Gator saying something about a turn in conv: that an agent is paused, that a chain
// stopped. It is a system line, like the one a promotion out of a DM leaves.
func (v *Voice) Announce(ctx context.Context, conv db.Conversation, body string) (db.Message, error) {
	var msg db.Message
	err := store.InTx(ctx, v.pool, func(q *db.Queries) error {
		var err error
		msg, err = postSystemText(ctx, q, conv, body)
		return err
	})
	return msg, err
}

// Join puts an agent in a conversation it was asked in, for people to see it there.
func (v *Voice) Join(ctx context.Context, conv, agent pgtype.UUID) error {
	return store.InTx(ctx, v.pool, func(q *db.Queries) error {
		_, err := joinAgent(ctx, q, conv, agent)
		return err
	})
}
