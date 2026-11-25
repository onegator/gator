package plugin

import "encoding/json"

// Messaging is the capability of a bridge: a plugin that carries what is said in a team's chat
// (Slack, then Teams, Telegram) or a code host's comments into a project's conversations, and what
// is said in them back out. The core has no code for any of them; it has these methods and hooks.
//
// What a bridge brings in is somebody else's words. It is kept apart as such: written by an
// outside identity (author kind "external") unless the person linked their account with a code
// they made in Gator, read by agents as untrusted data, and never an approval or a permission for
// an agent to work. Everything a bridge does is in its own project and on what it bridged itself.
const CapabilityMessaging = "messaging"

// Messaging hooks (core → plugin).
const (
	// MethodMessagePosted is a message in a conversation the bridge connects, to mirror outside.
	// The plugin answers the outside reference of the copy it made, if it made one; a message the
	// bridge brought in itself is never sent back to it.
	MethodMessagePosted = "messagePosted"
	// MethodMessageEdited is a message that changed: an agent's answer as it streams (at most
	// every couple of seconds, and always once it is final), a card whose state moved.
	MethodMessageEdited = "messageEdited"
)

// Messaging core methods (plugin → core).
const (
	CoreConversationFindOrCreate = "conversation.find_or_create"
	CoreMessagePost              = "message.post"
	CoreMessageUpdate            = "message.update"
	CoreMemberLink               = "member.link"
	CoreCardAction               = "card.action"
)

// Conversation kinds a bridge connects.
const (
	BridgeChannel = "channel" // a channel of the project, by name
	BridgeThread  = "thread"  // the thread under a message the bridge brought in or mirrored
	BridgeTask    = "task"    // a task's own thread
)

// ConversationFindOrCreateParams finds the conversation an outside place is connected to, or
// connects it. ExternalRef names the place in the plugin's own terms (T0123/C0456, a thread's
// T0123/C0456/1700000000.000100, owner/name#12).
//
//   - channel: the project's channel called Name, made if there is none. A private channel is
//     never connected: what is said in it would leave through the bridge.
//   - thread: the thread under the message whose outside reference is ParentRef, a message the
//     bridge itself brought in or mirrored.
//   - task: the thread of the task TaskID.
type ConversationFindOrCreateParams struct {
	ExternalRef string `json:"external_ref"`
	Kind        string `json:"kind"`
	Name        string `json:"name,omitempty"`
	Topic       string `json:"topic,omitempty"`
	ParentRef   string `json:"parent_ref,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
}

// ConversationFindOrCreateResult is the connected conversation.
type ConversationFindOrCreateResult struct {
	ConversationID string `json:"conversation_id"`
	Created        bool   `json:"created"`
}

// ExternalAuthor is who wrote something outside, as the plugin knows them. ExternalID is in the
// plugin's own namespace and must say whose account it is everywhere it could be confused with
// another: a Slack user is its team and user id (T0123/U0456), never the user id alone.
type ExternalAuthor struct {
	ExternalID  string `json:"external_id"`
	DisplayName string `json:"display_name,omitempty"`
}

// MessagePostParams posts what somebody said outside into a conversation the bridge connects.
// ExternalRef makes it idempotent: the same reference posted again answers the message it made.
// Mentions are agent handles the message asks (without the @): asking an agent from outside is
// asking it in chat, whatever it could do for a person.
type MessagePostParams struct {
	ConversationID string         `json:"conversation_id"`
	Body           string         `json:"body"`
	Author         ExternalAuthor `json:"author"`
	ExternalRef    string         `json:"external_ref"`
	Mentions       []string       `json:"mentions,omitempty"`
}

// MessagePostResult is the message, and whether this call made it.
type MessagePostResult struct {
	MessageID string `json:"message_id"`
	Replayed  bool   `json:"replayed,omitempty"`
	// AuthorKind is "user" when the author linked their account and may write here, else
	// "external".
	AuthorKind string `json:"author_kind"`
}

// MessageUpdateParams changes what a message the bridge brought in says, when its author edited it
// outside. Author is who edited it: only the account that wrote it changes it.
type MessageUpdateParams struct {
	ExternalRef string         `json:"external_ref"`
	Body        string         `json:"body"`
	Author      ExternalAuthor `json:"author"`
}

// MemberLinkParams links an outside account to the Gator account of the person who made Code (in
// Gator, for this project and this plugin), or, without a code, answers who it is linked to. An
// email address is never proof: whoever runs a workspace outside can give an account any address.
type MemberLinkParams struct {
	ExternalID  string `json:"external_id"`
	DisplayName string `json:"display_name,omitempty"`
	Code        string `json:"code,omitempty"`
}

// MemberLinkResult is the account the outside one is linked to; empty when none.
type MemberLinkResult struct {
	UserID string `json:"user_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Linked bool   `json:"linked"`
}

// CardActionParams is a button pressed outside on a card the bridge mirrored (Approve in Slack).
// It does what the same button in Gator does, for the person the outside account is linked to,
// with their rights; an account linked to nobody presses nothing.
type CardActionParams struct {
	MessageID string         `json:"message_id"`
	Action    string         `json:"action"`
	Actor     ExternalAuthor `json:"actor"`
}

// CardActionResult says what happened, in a sentence for the person who pressed.
type CardActionResult struct {
	Done    bool   `json:"done"`
	Message string `json:"message"`
}

// BridgedConversation is a conversation as a bridge sees it.
type BridgedConversation struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // channel | thread | task
	Name string `json:"name,omitempty"`
	// ExternalRef is where this conversation lives outside, on this plugin; empty when it lives
	// there only through the conversation it hangs under (ParentRef).
	ExternalRef string `json:"external_ref,omitempty"`
	// ParentRef is the outside reference of the message a thread hangs under, and RootRef that of
	// the conversation the thread's root message is in.
	ParentRef string `json:"parent_ref,omitempty"`
	RootRef   string `json:"root_ref,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	URL       string `json:"url,omitempty"` // where it reads in Gator
}

// BridgedMessage is a message as a bridge mirrors it.
type BridgedMessage struct {
	ID         string `json:"id"`
	Seq        int64  `json:"seq"`
	Kind       string `json:"kind"`   // text | card
	Status     string `json:"status"` // final | streaming | failed
	AuthorKind string `json:"author_kind"`
	// AuthorName is who wrote it, as Gator names them: a person's name, @handle for an agent.
	AuthorName string `json:"author_name"`
	Body       string `json:"body"`
	// External marks words that came from outside the workspace (another bridge, an A2A agent).
	External bool `json:"external,omitempty"`
	// CardType and Card are a card's kind and data; Actions are the buttons a bridge may offer
	// (card.action answers them).
	CardType string          `json:"card_type,omitempty"`
	Card     json.RawMessage `json:"card,omitempty"`
	Actions  []string        `json:"actions,omitempty"`
	// ExternalRef is this message's copy outside, on this plugin, once there is one.
	ExternalRef string `json:"external_ref,omitempty"`
	// ReplyTo is the outside reference of the message an agent's answer answers, when that message
	// came in through this plugin: a code host mirrors only those.
	ReplyTo string `json:"reply_to,omitempty"`
	URL     string `json:"url,omitempty"`
}

// MessageHookParams is messagePosted and messageEdited.
type MessageHookParams struct {
	Conversation BridgedConversation `json:"conversation"`
	Message      BridgedMessage      `json:"message"`
}

// MessageHookResult is the outside reference of the copy the plugin made or changed.
type MessageHookResult struct {
	ExternalRef string `json:"external_ref,omitempty"`
}

// WebhookResult is what a webhook answers the sender, for a sender that reads the answer (Slack's
// URL verification echoes a challenge). Status is 2xx; the body is at most 64 KiB.
type WebhookResult struct {
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"` // application/json (default) or text/plain
	Body        string `json:"body,omitempty"`
}
