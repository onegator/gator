package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// Bridged writes what a bridge carries in from outside (a Slack message, a comment on a pull
// request) into a conversation, as the person or the outside identity who said it. Whoever holds
// it has decided already that this may be said here: the bridges package, for a conversation the
// bridge connects, in its own project.
type Bridged struct {
	pool *pgxpool.Pool
}

// NewBridged builds it.
func NewBridged(pool *pgxpool.Pool) *Bridged { return &Bridged{pool: pool} }

// BridgedPost is one message said outside.
type BridgedPost struct {
	Conversation db.Conversation
	// Author is the person the outside account is linked to (AuthorUser), or the outside identity
	// itself (AuthorExternal, the external_identities row).
	Author Author
	Body   string
	// Mentions are the agents it asks.
	Mentions []Member
	// Provider is the bridge (the plugin's name) and ExternalRef the message outside: the same
	// reference again answers the message it made.
	Provider    string
	ExternalRef string
}

// validate holds a bridged post to what a person's is held to.
func (p BridgedPost) validate() error {
	switch {
	case strings.TrimSpace(p.Body) == "":
		return invalid("a message needs a body")
	case utf8.RuneCountInString(p.Body) > maxBodyRunes:
		return invalid("a message is at most %d characters", maxBodyRunes)
	case len(p.Mentions) > maxMentions:
		return invalid("a message mentions at most %d members", maxMentions)
	case p.Author.Kind != AuthorUser && p.Author.Kind != AuthorExternal, !p.Author.ID.Valid:
		return invalid("a bridged message is a person's or an outside identity's")
	case p.Provider == "" || p.ExternalRef == "":
		return invalid("a bridged message names its bridge and where it is outside")
	}
	return nil
}

// Post writes the message, or answers the one its outside reference already made. The reference
// is looked for under the conversation's row lock, so a redelivered event racing the first one
// writes nothing twice.
func (b *Bridged) Post(ctx context.Context, p BridgedPost) (db.Message, bool, error) {
	p.Body = strings.TrimSpace(p.Body)
	if err := p.validate(); err != nil {
		return db.Message{}, false, err
	}
	conv := p.Conversation
	origin, _ := json.Marshal(Origin{Provider: p.Provider, ExternalRef: p.ExternalRef})
	var msg db.Message
	err := store.InTxRetry(ctx, b.pool, func(q *db.Queries) error {
		seq, err := q.NextMessageSeq(ctx, conv.ID)
		if err != nil {
			return err
		}
		ref := db.GetExternalMessageParams{ProjectID: conv.ProjectID, Provider: p.Provider, ExternalRef: p.ExternalRef}
		if prior, err := q.GetExternalMessage(ctx, ref); err == nil {
			m, err := q.GetMessage(ctx, prior.MessageID)
			if err != nil {
				return err
			}
			return replay{m}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if msg, err = q.InsertMessage(ctx, db.InsertMessageParams{
			ConversationID: conv.ID, Seq: seq, AuthorKind: p.Author.Kind, AuthorID: p.Author.ID, Kind: messageText,
			Body: p.Body, Origin: origin, Status: messageFinal,
		}); err != nil {
			return err
		}
		if _, err := q.InsertExternalMessage(ctx, db.InsertExternalMessageParams{ProjectID: conv.ProjectID, Provider: p.Provider,
			ExternalRef: p.ExternalRef, MessageID: msg.ID}); err != nil {
			return err
		}
		for _, mm := range p.Mentions {
			if err := q.AddMention(ctx, db.AddMentionParams{MessageID: msg.ID, MemberKind: mm.Kind, MemberID: mm.ID}); err != nil {
				return err
			}
		}
		return afterPost(ctx, q, conv, msg, p.Author, p.Mentions)
	})
	var again replay
	if errors.As(err, &again) {
		return again.msg, true, nil
	}
	return msg, false, err
}

// Edit changes what a message the bridge brought in says, when its author edited it outside. Only
// a text message that came in through this bridge is edited; the same words again change nothing.
func (b *Bridged) Edit(ctx context.Context, project pgtype.UUID, provider, externalRef, body string) (db.Message, error) {
	body = strings.TrimSpace(body)
	if body == "" || utf8.RuneCountInString(body) > maxBodyRunes {
		return db.Message{}, invalid("a message is 1–%d characters", maxBodyRunes)
	}
	var out db.Message
	err := store.InTx(ctx, b.pool, func(q *db.Queries) error {
		ref, err := q.GetExternalMessage(ctx, db.GetExternalMessageParams{ProjectID: project, Provider: provider, ExternalRef: externalRef})
		if err != nil {
			return found(err)
		}
		msg, err := q.GetMessageForUpdate(ctx, ref.MessageID)
		if err != nil {
			return found(err)
		}
		if msg.Kind != messageText || msg.DeletedAt.Valid || OriginOf(msg).Provider != provider {
			return ErrNotFound // mirrored out from here, or gone: not the bridge's to change
		}
		if out = msg; msg.Body == body {
			return nil
		}
		if out, err = q.EditMessage(ctx, db.EditMessageParams{ID: msg.ID, Body: body}); err != nil {
			return err
		}
		return emit(ctx, q, EventMessageEdited, out.ConversationID, messagePayload(out, nil))
	})
	return out, err
}

// BridgeChannel answers the project's channel called name, making it as Gator when there is
// none. A private or archived channel is not one a bridge may speak in.
func (b *Bridged) BridgeChannel(ctx context.Context, project pgtype.UUID, name, topic string) (db.Conversation, bool, error) {
	n, err := normalizeChannelName(name)
	if err != nil {
		return db.Conversation{}, false, err
	}
	var conv db.Conversation
	created := false
	err = store.InTx(ctx, b.pool, func(q *db.Queries) error {
		made, err := q.CreateChannelIfMissing(ctx, db.CreateChannelIfMissingParams{ProjectID: project, Name: n, Topic: strings.TrimSpace(topic)})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			conv, err = q.GetChannelByName(ctx, db.GetChannelByNameParams{ProjectID: project, Name: n})
			return err
		case err != nil:
			return err
		}
		conv, created = made, true
		return emit(ctx, q, EventConversationCreated, conv.ID, map[string]any{"kind": conv.Kind, "projectId": uuidString(project)})
	})
	if err != nil {
		return db.Conversation{}, false, err
	}
	if conv.Visibility != VisibilityProject || conv.ArchivedAt.Valid {
		return db.Conversation{}, false, conflict("#%s is private or archived; a bridge speaks only in channels open to the project", n)
	}
	return conv, created, nil
}
