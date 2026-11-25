package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/onegator/gator/plugin"
	"github.com/onegator/gator/plugin/netguard"
)

const configSchema = `{"type":"object","properties":{
	"bot_token":{"type":"string","x-secret":true,"title":"Bot token","description":"set by Connect Slack (xoxb-…)"},
	"signing_secret":{"type":"string","x-secret":true,"title":"Signing secret","description":"from the Slack app's Basic Information; every request from Slack is checked against it"},
	"channels":{"type":"string","title":"Channels","description":"Slack channel ids and the project channels they are, comma separated: C0123ABC=eng,C0456DEF=ops. Only these are bridged; a private Gator channel never is"},
	"api_url":{"type":"string","title":"API address","description":"Web API base; default https://slack.com/api"}
	},"required":["channels"]}`

var manifest = plugin.Manifest{
	Name:    "slack",
	Version: "0.1.0",
	Description: "Talk to the project's agents from Slack: connected channels and their threads are the project's " +
		"conversations, answers come back in the thread, and decisions can be approved by a linked account.",
	Capabilities: []string{plugin.CapabilityMessaging},
	Links:        true, // /gator link <code>
	ConfigSchema: json.RawMessage(configSchema),
	// Slack signs a request but names no delivery; the core takes the body's hash, which a retry
	// repeats, and every message is posted once by its ts.
	Connect: &plugin.ConnectSpec{Title: "Slack", AuthorizeURL: "https://slack.com/oauth/v2/authorize",
		TokenURL: "https://slack.com/api/oauth.v2.access", ScopeSeparator: ",",
		Scopes: []string{"app_mentions:read", "channels:history", "channels:read", "chat:write", "commands", "users:read"},
		Secret: "bot_token"},
}

// Limits of what goes out: a section of Block Kit holds 3000 characters, a message 40000.
const (
	maxSection = 3000
	maxText    = 39000
)

// slack is the bridge. It knows its workspace from its own token (auth.test) and refuses anything
// signed for it that comes from another.
type slack struct {
	http *http.Client
	now  func() time.Time

	mu    sync.Mutex
	me    *Identity
	names map[string]string // Slack user → display name
}

func (s *slack) handlers() plugin.Handlers {
	return plugin.Handlers{Manifest: manifest, WebhookReply: s.webhook, MessagePosted: s.posted, MessageEdited: s.edited}
}

func (s *slack) client(core *plugin.Core) *Client {
	// The bot token goes to the API address, which a project admin sets: only Slack's unless the
	// operator allowed another host (GATOR_PLUGIN_API_HOSTS).
	base, err := netguard.APIBase(core.Setting("api_url"), "https://slack.com/api", os.Getenv)
	return &Client{Base: base, Err: err, Token: core.Secret("bot_token"), HTTP: s.http}
}

// identity is who the bot is, asked once.
func (s *slack) identity(ctx context.Context, core *plugin.Core) (Identity, error) {
	s.mu.Lock()
	if s.me != nil {
		defer s.mu.Unlock()
		return *s.me, nil
	}
	s.mu.Unlock()
	id, err := s.client(core).AuthTest(ctx)
	if err != nil {
		return Identity{}, err
	}
	if id.TeamID == "" || id.UserID == "" {
		return Identity{}, fmt.Errorf("slack auth.test named no team")
	}
	s.mu.Lock()
	s.me = &id
	s.mu.Unlock()
	return id, nil
}

// channels are the Slack channels the project bridges and the Gator channels they are.
func channels(core *plugin.Core) map[string]string {
	out := map[string]string{}
	for pair := range strings.SplitSeq(core.Setting("channels"), ",") {
		id, name, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && strings.TrimSpace(id) != "" && strings.TrimSpace(name) != "" {
			out[strings.TrimSpace(id)] = strings.TrimPrefix(strings.TrimSpace(name), "#")
		}
	}
	return out
}

// Outside references: the team in every one, so nothing of one workspace is mistaken for another's.
func userRef(team, user string) string           { return team + "/" + user }
func channelRef(team, channel string) string     { return team + "/" + channel }
func messageRef(team, channel, ts string) string { return team + "/" + channel + "/" + ts }

// splitRef reads a message's or a channel's reference.
func splitRef(ref string) (team, channel, ts string) {
	parts := strings.SplitN(ref, "/", 3)
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return parts[0], parts[1], ""
	}
	return "", "", ""
}

// webhook takes what Slack sends: events, button presses, slash commands. Nothing is read before
// its signature and its age are checked.
func (s *slack) webhook(ctx context.Context, core *plugin.Core, p plugin.WebhookParams) (plugin.WebhookResult, error) {
	if !verify(core.Secret("signing_secret"), p.Headers["X-Slack-Request-Timestamp"], p.Headers["X-Slack-Signature"], p.Body, s.now()) {
		return plugin.WebhookResult{}, plugin.Errorf(plugin.CodeForbidden, "bad or stale Slack signature")
	}
	if strings.HasPrefix(p.Headers["Content-Type"], "application/x-www-form-urlencoded") {
		form, err := url.ParseQuery(string(p.Body))
		if err != nil {
			return plugin.WebhookResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unreadable form")
		}
		if payload := form.Get("payload"); payload != "" {
			return s.interaction(ctx, core, []byte(payload))
		}
		if form.Get("command") != "" {
			return s.command(ctx, core, form)
		}
		return plugin.WebhookResult{}, plugin.Errorf(plugin.CodeInvalidParams, "a form that is neither a button nor a command")
	}
	var env struct {
		Type      string          `json:"type"`
		Challenge string          `json:"challenge"`
		TeamID    string          `json:"team_id"`
		Event     json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(p.Body, &env); err != nil {
		return plugin.WebhookResult{}, plugin.Errorf(plugin.CodeInvalidParams, "not JSON")
	}
	switch env.Type {
	case "url_verification":
		b, _ := json.Marshal(map[string]string{"challenge": env.Challenge})
		return plugin.WebhookResult{Body: string(b)}, nil
	case "event_callback":
		me, err := s.ours(ctx, core, env.TeamID)
		if err != nil {
			return plugin.WebhookResult{}, err
		}
		return plugin.WebhookResult{}, s.event(ctx, core, me, env.Event)
	}
	return plugin.WebhookResult{}, nil // another kind of callback: nothing to do
}

// ours is the bot's identity when team is its workspace, and a refusal otherwise: a request signed
// with this app's secret for another workspace is not this project's.
func (s *slack) ours(ctx context.Context, core *plugin.Core, team string) (Identity, error) {
	me, err := s.identity(ctx, core)
	if err != nil {
		return Identity{}, err
	}
	if team != me.TeamID {
		return Identity{}, plugin.Errorf(plugin.CodeForbidden, "this request is for another Slack workspace")
	}
	return me, nil
}

type message struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	Channel  string `json:"channel"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	Message  *struct {
		User  string `json:"user"`
		BotID string `json:"bot_id"`
		Text  string `json:"text"`
		TS    string `json:"ts"`
	} `json:"message"`
}

// event is one message in a bridged channel: posted (or edited) into its conversation, a thread's
// reply into the thread's.
func (s *slack) event(ctx context.Context, core *plugin.Core, me Identity, raw json.RawMessage) error {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		return plugin.Errorf(plugin.CodeInvalidParams, "unreadable event")
	}
	if m.Type != "message" && m.Type != "app_mention" {
		return nil
	}
	name, bridged := channels(core)[m.Channel]
	if !bridged {
		return nil
	}
	if m.Subtype == "message_changed" && m.Message != nil {
		if m.Message.BotID != "" || m.Message.User == me.UserID || m.Message.User == "" {
			return nil
		}
		err := core.UpdateMessage(ctx, plugin.MessageUpdateParams{ExternalRef: messageRef(me.TeamID, m.Channel, m.Message.TS),
			Body: plain(m.Message.Text, me.UserID), Author: plugin.ExternalAuthor{ExternalID: userRef(me.TeamID, m.Message.User)}})
		if pe, ok := err.(*plugin.Error); ok && pe.Code == plugin.CodeNotFound {
			return nil // a message from before the bridge
		}
		return err
	}
	// The bridge's own messages come back as events; so do other bots', and joins and leaves.
	if m.Subtype != "" || m.BotID != "" || m.User == "" || m.User == me.UserID {
		return nil
	}
	conv, err := core.FindOrCreateConversation(ctx, plugin.ConversationFindOrCreateParams{
		ExternalRef: channelRef(me.TeamID, m.Channel), Kind: plugin.BridgeChannel, Name: name})
	if err != nil {
		return err
	}
	if m.ThreadTS != "" && m.ThreadTS != m.TS {
		conv, err = core.FindOrCreateConversation(ctx, plugin.ConversationFindOrCreateParams{
			ExternalRef: messageRef(me.TeamID, m.Channel, m.ThreadTS), Kind: plugin.BridgeThread,
			ParentRef: messageRef(me.TeamID, m.Channel, m.ThreadTS)})
		if pe, ok := err.(*plugin.Error); ok && pe.Code == plugin.CodeNotFound {
			return nil // a thread under a message from before the bridge
		}
		if err != nil {
			return err
		}
	}
	body := plain(m.Text, me.UserID)
	if body == "" {
		return nil
	}
	_, err = core.PostMessage(ctx, plugin.MessagePostParams{ConversationID: conv.ConversationID, Body: body,
		Author:      plugin.ExternalAuthor{ExternalID: userRef(me.TeamID, m.User), DisplayName: s.userName(ctx, core, m.User)},
		ExternalRef: messageRef(me.TeamID, m.Channel, m.TS), Mentions: handles(m.Text, me.UserID)})
	return err
}

// userName is a Slack user's display name, asked once per process; a failed lookup is the id.
func (s *slack) userName(ctx context.Context, core *plugin.Core, user string) string {
	s.mu.Lock()
	if n, ok := s.names[user]; ok {
		s.mu.Unlock()
		return n
	}
	s.mu.Unlock()
	n, err := s.client(core).UserName(ctx, user)
	if err != nil {
		return user
	}
	s.mu.Lock()
	if len(s.names) > 5000 {
		s.names = map[string]string{}
	}
	s.names[user] = n
	s.mu.Unlock()
	return n
}

// command is /gator: "link <code>" links the account, "ask @agent …" asks in the channel.
func (s *slack) command(ctx context.Context, core *plugin.Core, form url.Values) (plugin.WebhookResult, error) {
	me, err := s.ours(ctx, core, form.Get("team_id"))
	if err != nil {
		return plugin.WebhookResult{}, err
	}
	user, channel := form.Get("user_id"), form.Get("channel_id")
	verb, rest, _ := strings.Cut(strings.TrimSpace(form.Get("text")), " ")
	rest = strings.TrimSpace(rest)
	switch strings.ToLower(verb) {
	case "link":
		r, err := core.LinkMember(ctx, plugin.MemberLinkParams{ExternalID: userRef(me.TeamID, user), DisplayName: form.Get("user_name"), Code: rest})
		if pe, ok := err.(*plugin.Error); ok && pe.Code == plugin.CodeForbidden {
			return ephemeral(pe.Message), nil
		}
		if err != nil {
			return plugin.WebhookResult{}, err
		}
		if !r.Linked {
			return ephemeral("Your Slack account is not linked. Make a code in Gator (project settings, Link Slack) and type /gator link <code>."), nil
		}
		return ephemeral("Linked to " + r.Name + " in Gator."), nil
	case "ask":
		name, bridged := channels(core)[channel]
		if !bridged {
			return ephemeral("This channel is not connected to Gator."), nil
		}
		if rest == "" {
			return ephemeral("Ask what? /gator ask @reviewer look at #12"), nil
		}
		// The question becomes a Slack message of the bridge's, so the answer has a thread to go to.
		text := fmt.Sprintf("<@%s> asked: %s", user, escape(rest))
		ts, err := s.client(core).Post(ctx, channel, "", text, nil)
		if err != nil {
			return plugin.WebhookResult{}, err
		}
		conv, err := core.FindOrCreateConversation(ctx, plugin.ConversationFindOrCreateParams{
			ExternalRef: channelRef(me.TeamID, channel), Kind: plugin.BridgeChannel, Name: name})
		if err != nil {
			return plugin.WebhookResult{}, err
		}
		if _, err := core.PostMessage(ctx, plugin.MessagePostParams{ConversationID: conv.ConversationID, Body: rest,
			Author:      plugin.ExternalAuthor{ExternalID: userRef(me.TeamID, user), DisplayName: s.userName(ctx, core, user)},
			ExternalRef: messageRef(me.TeamID, channel, ts), Mentions: handles(rest, "")}); err != nil {
			return plugin.WebhookResult{}, err
		}
		return plugin.WebhookResult{}, nil
	}
	return ephemeral("/gator link <code> links your account; /gator ask @agent <question> asks an agent here."), nil
}

func ephemeral(text string) plugin.WebhookResult {
	b, _ := json.Marshal(map[string]string{"response_type": "ephemeral", "text": text})
	return plugin.WebhookResult{Body: string(b)}
}

// approveAction is the action id of a card's Approve button.
const approveAction = "gator_approve"

// interaction is a button pressed on a card the bridge drew.
func (s *slack) interaction(ctx context.Context, core *plugin.Core, raw []byte) (plugin.WebhookResult, error) {
	var p struct {
		Type string `json:"type"`
		Team struct {
			ID string `json:"id"`
		} `json:"team"`
		User struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"user"`
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
		Actions []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return plugin.WebhookResult{}, plugin.Errorf(plugin.CodeInvalidParams, "unreadable interaction")
	}
	if p.Type != "block_actions" {
		return plugin.WebhookResult{}, nil
	}
	me, err := s.ours(ctx, core, p.Team.ID)
	if err != nil {
		return plugin.WebhookResult{}, err
	}
	for _, a := range p.Actions {
		if a.ActionID != approveAction {
			continue
		}
		r, err := core.CardAction(ctx, plugin.CardActionParams{MessageID: a.Value, Action: "approve",
			Actor: plugin.ExternalAuthor{ExternalID: userRef(me.TeamID, p.User.ID), DisplayName: p.User.Name}})
		if err != nil {
			return plugin.WebhookResult{}, err
		}
		if err := s.client(core).Ephemeral(ctx, p.Channel.ID, p.User.ID, r.Message); err != nil {
			return plugin.WebhookResult{}, err
		}
	}
	return plugin.WebhookResult{}, nil
}
