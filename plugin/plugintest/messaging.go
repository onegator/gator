package plugintest

import (
	"encoding/json"
	"strings"

	"github.com/onegator/gator/plugin"
)

// Conversation is a conversation a bridge connected in the fake core.
type Conversation struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name,omitempty"`
	ExternalRef string `json:"external_ref"`
	ParentRef   string `json:"parent_ref,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
}

// Message is a message a bridge posted in the fake core.
type Message struct {
	ID             string   `json:"id"`
	ConversationID string   `json:"conversation_id"`
	Body           string   `json:"body"`
	AuthorKind     string   `json:"author_kind"` // user when linked, else external
	Author         string   `json:"author"`      // the external id
	ExternalRef    string   `json:"external_ref"`
	Mentions       []string `json:"mentions,omitempty"`
}

// messaging is the fake core's side of bridges: conversations by outside reference, messages by
// outside reference, and links made with LinkCode.
type messaging struct {
	conversations []Conversation
	messages      []*Message
	codes         map[string]string // code → user id
	links         map[string]string // external id → user id
	actions       []plugin.CardActionParams
}

func (m *messaging) init() {
	if m.codes == nil {
		m.codes, m.links = map[string]string{}, map[string]string{}
	}
}

// LinkCode makes a code that links an account to userID, as a person does in Gator.
func (c *Core) LinkCode(code, userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msg.init()
	c.msg.codes[strings.ToUpper(code)] = userID
}

// Conversations are what the bridge connected.
func (c *Core) Conversations() []Conversation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Conversation(nil), c.msg.conversations...)
}

// Messages are what the bridge posted.
func (c *Core) Messages() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, len(c.msg.messages))
	for i, m := range c.msg.messages {
		out[i] = *m
	}
	return out
}

// CardActions are the buttons the bridge pressed.
func (c *Core) CardActions() []plugin.CardActionParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]plugin.CardActionParams(nil), c.msg.actions...)
}

// handleMessaging answers the messaging core methods; ok is false for any other method.
func (c *Core) handleMessaging(method string, raw json.RawMessage) (any, bool, error) {
	m := &c.msg
	m.init()
	switch method {
	case plugin.CoreConversationFindOrCreate:
		p, err := decode[plugin.ConversationFindOrCreateParams](raw)
		if err != nil {
			return nil, true, err
		}
		if p.ExternalRef == "" {
			return nil, true, plugin.Errorf(plugin.CodeInvalidParams, "external_ref is required")
		}
		for _, x := range m.conversations {
			if x.ExternalRef == p.ExternalRef {
				return plugin.ConversationFindOrCreateResult{ConversationID: x.ID}, true, nil
			}
		}
		switch p.Kind {
		case plugin.BridgeChannel, plugin.BridgeThread, plugin.BridgeTask:
		default:
			return nil, true, plugin.Errorf(plugin.CodeInvalidParams, "kind is channel, thread or task")
		}
		if p.Kind == plugin.BridgeTask && c.find(p.TaskID) == nil {
			return nil, true, plugin.Errorf(plugin.CodeNotFound, "task %s not found", p.TaskID)
		}
		conv := Conversation{ID: newID(), Kind: p.Kind, Name: p.Name, ExternalRef: p.ExternalRef, ParentRef: p.ParentRef, TaskID: p.TaskID}
		m.conversations = append(m.conversations, conv)
		return plugin.ConversationFindOrCreateResult{ConversationID: conv.ID, Created: true}, true, nil
	case plugin.CoreMessagePost:
		p, err := decode[plugin.MessagePostParams](raw)
		if err != nil {
			return nil, true, err
		}
		if strings.TrimSpace(p.Body) == "" || p.ExternalRef == "" || p.Author.ExternalID == "" {
			return nil, true, plugin.Errorf(plugin.CodeInvalidParams, "body, external_ref and author.external_id are required")
		}
		known := false
		for _, x := range m.conversations {
			known = known || x.ID == p.ConversationID
		}
		if !known {
			return nil, true, plugin.Errorf(plugin.CodeNotFound, "conversation not found")
		}
		for _, x := range m.messages {
			if x.ExternalRef == p.ExternalRef {
				return plugin.MessagePostResult{MessageID: x.ID, Replayed: true, AuthorKind: x.AuthorKind}, true, nil
			}
		}
		kind := "external"
		if m.links[p.Author.ExternalID] != "" {
			kind = "user"
		}
		msg := &Message{ID: newID(), ConversationID: p.ConversationID, Body: p.Body, AuthorKind: kind, Author: p.Author.ExternalID,
			ExternalRef: p.ExternalRef, Mentions: p.Mentions}
		m.messages = append(m.messages, msg)
		return plugin.MessagePostResult{MessageID: msg.ID, AuthorKind: kind}, true, nil
	case plugin.CoreMessageUpdate:
		p, err := decode[plugin.MessageUpdateParams](raw)
		if err != nil {
			return nil, true, err
		}
		for _, x := range m.messages {
			if x.ExternalRef == p.ExternalRef {
				x.Body = p.Body
				return nil, true, nil
			}
		}
		return nil, true, plugin.Errorf(plugin.CodeNotFound, "no message %q came in through this bridge", p.ExternalRef)
	case plugin.CoreMemberLink:
		p, err := decode[plugin.MemberLinkParams](raw)
		if err != nil {
			return nil, true, err
		}
		if p.Code != "" {
			user, ok := m.codes[strings.ToUpper(strings.TrimSpace(p.Code))]
			if !ok {
				return nil, true, plugin.Errorf(plugin.CodeForbidden, "that code is not one")
			}
			delete(m.codes, strings.ToUpper(strings.TrimSpace(p.Code)))
			m.links[p.ExternalID] = user
		}
		if u := m.links[p.ExternalID]; u != "" {
			return plugin.MemberLinkResult{UserID: u, Linked: true}, true, nil
		}
		return plugin.MemberLinkResult{}, true, nil
	case plugin.CoreCardAction:
		p, err := decode[plugin.CardActionParams](raw)
		if err != nil {
			return nil, true, err
		}
		m.actions = append(m.actions, p)
		if m.links[p.Actor.ExternalID] == "" {
			return plugin.CardActionResult{Message: "Only a person with a Gator account linked here can do that."}, true, nil
		}
		return plugin.CardActionResult{Done: true, Message: "Approved."}, true, nil
	}
	return nil, false, nil
}
