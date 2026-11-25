package bridges

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/server/words"
	"github.com/onegator/gator/plugin"
)

// EditEvery is how often an edit of a message still being written goes out to a bridge: an agent's
// answer streams into its message every half second, and a chat's API is not a terminal. The final
// edit always goes.
const EditEvery = 2 * time.Second

type editKey struct {
	message  [16]byte
	provider string
}

// HookCall calls a plugin's hook; the plugins host gives one for each plugin.
type HookCall = func(ctx context.Context, method string, params, out any) error

// Methods are the messaging core methods, for the plugins host to serve to plugins with the
// messaging capability, each call as the plugin and project it came from.
func (s *Service) Methods() map[string]func(ctx context.Context, project pgtype.UUID, provider string, raw json.RawMessage) (any, error) {
	return map[string]func(ctx context.Context, project pgtype.UUID, provider string, raw json.RawMessage) (any, error){
		plugin.CoreConversationFindOrCreate: serve(s.FindOrCreate),
		plugin.CoreMessagePost:              serve(s.Post),
		plugin.CoreMessageUpdate: serve(func(ctx context.Context, b Bridge, p plugin.MessageUpdateParams) (any, error) {
			return nil, s.Update(ctx, b, p)
		}),
		plugin.CoreMemberLink: serve(s.Link),
		plugin.CoreCardAction: serve(s.CardAction),
	}
}

func serve[P, R any](fn func(context.Context, Bridge, P) (R, error)) func(context.Context, pgtype.UUID, string, json.RawMessage) (any, error) {
	return func(ctx context.Context, project pgtype.UUID, provider string, raw json.RawMessage) (any, error) {
		var p P
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, invalid("%v", err)
		}
		return fn(ctx, Bridge{Project: project, Provider: provider}, p)
	}
}

// Events are the outbox events a bridge mirrors.
func (s *Service) Events() []string {
	return []string{conversations.EventMessagePosted, conversations.EventMessageEdited}
}

// Capability is what a plugin declares to hear them.
func (s *Service) Capability() string { return plugin.CapabilityMessaging }

// Project is the project a message event happened in.
func (s *Service) Project(ctx context.Context, e events.Event) (pgtype.UUID, bool, error) {
	id, ok := words.UUID(e.AggregateID)
	if !ok {
		return pgtype.UUID{}, false, nil
	}
	// A conversation never changes projects: every message event of it after the first is answered
	// without the database, which every message in the server passes through.
	s.mu.Lock()
	p, known := s.projects[id.Bytes]
	s.mu.Unlock()
	if known {
		return p, p.Valid, nil
	}
	conv, err := s.q.GetConversation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, false, nil
	}
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	s.mu.Lock()
	if len(s.projects) >= maxKnownConversations {
		clear(s.projects)
	}
	s.projects[id.Bytes] = conv.ProjectID
	s.mu.Unlock()
	return conv.ProjectID, conv.ProjectID.Valid, nil
}

// Deliver mirrors one message event to one bridge, when the message is in a conversation the
// bridge connects, is open to the project, and did not come in through that bridge. The copy's
// outside reference is kept, so that edits change it.
func (s *Service) Deliver(ctx context.Context, e events.Event, project pgtype.UUID, provider string, call HookCall) error {
	var p struct {
		MessageID string `json:"messageId"`
	}
	id, ok := pgtype.UUID{}, false
	if json.Unmarshal(e.Payload, &p) == nil {
		id, ok = words.UUID(p.MessageID)
	}
	if !ok {
		return nil
	}
	b := Bridge{Project: project, Provider: provider}
	params, ok, err := s.Outgoing(ctx, b, id)
	if err != nil || !ok {
		return err
	}
	method := plugin.MethodMessagePosted
	if e.Type == conversations.EventMessageEdited && params.Message.ExternalRef != "" {
		if !s.editDue(id, provider, params.Message.Status) {
			return nil
		}
		method = plugin.MethodMessageEdited
	} else if e.Type == conversations.EventMessageEdited && params.Message.Status == conversations.StatusStreaming {
		// Edited before its first copy went out (a bridge connected mid-answer): it goes out once
		// it is written, or at the next edit that is due.
		if !s.editDue(id, provider, params.Message.Status) {
			return nil
		}
	}
	var r plugin.MessageHookResult
	if err := call(ctx, method, params, &r); err != nil {
		return err
	}
	if r.ExternalRef == "" || r.ExternalRef == params.Message.ExternalRef {
		return nil
	}
	if !validRef(r.ExternalRef) {
		return fmt.Errorf("%s answered an external_ref that is not one", provider)
	}
	_, err = s.q.InsertExternalMessage(ctx, db.InsertExternalMessageParams{ProjectID: project, Provider: provider,
		ExternalRef: r.ExternalRef, MessageID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already known, or the outside reference names another message: keep the first
	}
	return err
}

// editDue says whether an edit of a message goes out now: always once it is no longer being
// written, else at most every EditEvery.
func (s *Service) editDue(id pgtype.UUID, provider, status string) bool {
	k := editKey{message: id.Bytes, provider: provider}
	s.mu.Lock()
	defer s.mu.Unlock()
	if status != conversations.StatusStreaming {
		delete(s.sent, k)
		return true
	}
	now := s.deps.Now()
	if last, ok := s.sent[k]; ok && now.Sub(last) < EditEvery {
		return false
	}
	s.sent[k] = now
	if len(s.sent) > 10000 { // answers that never finished; forget the oldest half-way
		for key, at := range s.sent {
			if now.Sub(at) > time.Hour {
				delete(s.sent, key)
			}
		}
	}
	return true
}

// Outgoing is what a bridge is told of a message, or nothing when it is not the bridge's to see.
func (s *Service) Outgoing(ctx context.Context, b Bridge, id pgtype.UUID) (plugin.MessageHookParams, bool, error) {
	msg, err := s.q.GetMessage(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return plugin.MessageHookParams{}, false, nil
	}
	if err != nil {
		return plugin.MessageHookParams{}, false, err
	}
	if msg.DeletedAt.Valid || conversations.OriginOf(msg).Provider == b.Provider {
		return plugin.MessageHookParams{}, false, nil // gone, or the bridge's own words: no echo
	}
	conv, err := s.bridged(ctx, b, msg.ConversationID)
	if err != nil {
		if pe, ok := errors.AsType[*plugin.Error](err); ok && pe.Code == plugin.CodeNotFound {
			return plugin.MessageHookParams{}, false, nil
		}
		return plugin.MessageHookParams{}, false, err
	}
	if open, err := s.deps.Rights.OpenToProject(ctx, conv); err != nil || !open {
		return plugin.MessageHookParams{}, false, err
	}
	bc, ok, err := s.conversationOut(ctx, b, conv)
	if err != nil || !ok {
		return plugin.MessageHookParams{}, false, err
	}
	bm, err := s.messageOut(ctx, b, msg)
	if err != nil {
		return plugin.MessageHookParams{}, false, err
	}
	return plugin.MessageHookParams{Conversation: bc, Message: bm}, true, nil
}

// conversationOut names a conversation as the bridge knows it; a thread under a message the
// bridge never saw has no place outside, and is not mirrored.
func (s *Service) conversationOut(ctx context.Context, b Bridge, conv db.Conversation) (plugin.BridgedConversation, bool, error) {
	out := plugin.BridgedConversation{ID: conversations.UUIDString(conv.ID), Kind: conv.Kind,
		TaskID: conversations.UUIDString(conv.TaskID), URL: "gator://conversations/" + conversations.UUIDString(conv.ID)}
	if conv.Name != nil {
		out.Name = *conv.Name
	}
	if ref, err := s.q.GetConversationRefOf(ctx, db.GetConversationRefOfParams{Provider: b.Provider, ConversationID: conv.ID}); err == nil {
		out.ExternalRef = ref.ExternalRef
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	if !conv.ParentMessageID.Valid {
		return out, out.ExternalRef != "", nil
	}
	parent, err := s.q.GetMessage(ctx, conv.ParentMessageID)
	if err != nil {
		return out, false, err
	}
	if ref, err := s.q.GetMessageRefOf(ctx, db.GetMessageRefOfParams{Provider: b.Provider, MessageID: parent.ID}); err == nil {
		out.ParentRef = ref.ExternalRef
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	if ref, err := s.q.GetConversationRefOf(ctx, db.GetConversationRefOfParams{Provider: b.Provider, ConversationID: parent.ConversationID}); err == nil {
		out.RootRef = ref.ExternalRef
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	return out, out.ExternalRef != "" || out.ParentRef != "", nil
}

// messageOut is a message as a bridge mirrors it.
func (s *Service) messageOut(ctx context.Context, b Bridge, msg db.Message) (plugin.BridgedMessage, error) {
	out := plugin.BridgedMessage{ID: conversations.UUIDString(msg.ID), Seq: msg.Seq, Kind: msg.Kind, Status: msg.Status,
		AuthorKind: msg.AuthorKind, Body: msg.Body, Actions: offered(msg),
		URL: fmt.Sprintf("gator://conversations/%s?seq=%d", conversations.UUIDString(msg.ConversationID), msg.Seq)}
	if msg.CardType != nil {
		out.CardType, out.Card = *msg.CardType, json.RawMessage(msg.Card)
	}
	name, external, err := s.authorName(ctx, msg)
	if err != nil {
		return out, err
	}
	out.AuthorName, out.External = name, external
	if ref, err := s.q.GetMessageRefOf(ctx, db.GetMessageRefOfParams{Provider: b.Provider, MessageID: msg.ID}); err == nil {
		out.ExternalRef = ref.ExternalRef
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	// An agent's answer says what it answers, when that came in through this bridge.
	if msg.AuthorKind == conversations.AuthorAgent {
		if turn, ok := words.UUID(conversations.OriginOf(msg).Turn); ok {
			t, err := s.q.GetTurn(ctx, turn)
			if err == nil && t.ReplyMessageID == msg.ID {
				if ref, err := s.q.GetMessageRefOf(ctx, db.GetMessageRefOfParams{Provider: b.Provider, MessageID: t.TriggerMessageID}); err == nil {
					out.ReplyTo = ref.ExternalRef
				}
			} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return out, err
			}
		}
	}
	return out, nil
}

// authorName says who wrote a message, and whether they are outside the workspace.
func (s *Service) authorName(ctx context.Context, msg db.Message) (string, bool, error) {
	switch msg.AuthorKind {
	case conversations.AuthorUser:
		u, err := s.q.GetUser(ctx, msg.AuthorID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "a person", false, nil
		}
		return u.Name, false, err
	case conversations.AuthorAgent:
		a, err := s.q.GetAgent(ctx, msg.AuthorID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "an agent", false, nil
		}
		return "@" + a.Handle, a.Kind == conversations.AgentRemote, err
	case conversations.AuthorExternal:
		id, err := s.q.GetExternalIdentity(ctx, msg.AuthorID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "someone outside", true, nil
		}
		return id.DisplayName + " (" + id.Provider + ")", true, err
	}
	return "Gator", false, nil
}
