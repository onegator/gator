package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/onegator/gator/plugin"
)

// block is one Block Kit block.
type block map[string]any

// place is where a message goes in Slack: a channel, and the thread it belongs to.
type place struct {
	team, channel, thread string
}

// placeOf reads where a conversation lives in Slack: a channel's own reference, a thread's (its
// root message's), or the message a Gator thread hangs under.
func placeOf(c plugin.BridgedConversation) (place, bool) {
	if c.ExternalRef != "" {
		team, channel, ts := splitRef(c.ExternalRef)
		return place{team: team, channel: channel, thread: ts}, channel != ""
	}
	if c.ParentRef != "" {
		team, channel, ts := splitRef(c.ParentRef)
		return place{team: team, channel: channel, thread: ts}, channel != "" && ts != ""
	}
	return place{}, false
}

// render is a Gator message as Slack shows it: who said it, what, escaped so that it pings nobody
// and links nowhere it did not say; a card with the buttons a bridge may offer.
func render(m plugin.BridgedMessage) (string, []block) {
	who := m.AuthorName
	if m.External {
		who += " (outside the workspace)"
	}
	body := m.Body
	// An answer still being written says so — unless it does already ("@reviewer is reading the
	// conversation…").
	if m.Status == "streaming" && !strings.HasSuffix(strings.TrimSpace(body), "…") {
		body += " …"
	}
	text := clip(fmt.Sprintf("*%s*: %s", escape(who), escape(body)), maxText)
	if m.Kind != "card" {
		return text, nil
	}
	blocks := []block{{"type": "section", "text": block{"type": "mrkdwn", "text": clip(escape(m.Body), maxSection)}}}
	var buttons []block
	for _, a := range m.Actions {
		if a == "approve" {
			buttons = append(buttons, block{"type": "button", "action_id": approveAction, "value": m.ID, "style": "primary",
				"text": block{"type": "plain_text", "text": "Approve"}})
		}
	}
	if len(buttons) > 0 {
		blocks = append(blocks, block{"type": "actions", "elements": buttons})
	}
	blocks = append(blocks, block{"type": "context", "elements": []block{{"type": "mrkdwn", "text": escape("In Gator: " + m.URL)}}})
	return clip(escape(m.Body), maxText), blocks
}

// posted mirrors a new message into its channel or thread, and answers the copy's reference.
func (s *slack) posted(ctx context.Context, core *plugin.Core, p plugin.MessageHookParams) (plugin.MessageHookResult, error) {
	where, ok := placeOf(p.Conversation)
	if !ok {
		return plugin.MessageHookResult{}, nil
	}
	if me, err := s.identity(ctx, core); err != nil {
		return plugin.MessageHookResult{}, err
	} else if where.team != me.TeamID {
		return plugin.MessageHookResult{}, nil // connected by another workspace's token: not ours to post in
	}
	text, blocks := render(p.Message)
	ts, err := s.client(core).Post(ctx, where.channel, where.thread, text, blocks)
	if err != nil {
		return plugin.MessageHookResult{}, err
	}
	return plugin.MessageHookResult{ExternalRef: messageRef(where.team, where.channel, ts)}, nil
}

// edited brings the copy up to date: an answer as it streams, a card whose decision was made.
func (s *slack) edited(ctx context.Context, core *plugin.Core, p plugin.MessageHookParams) (plugin.MessageHookResult, error) {
	if p.Message.ExternalRef == "" {
		return s.posted(ctx, core, p)
	}
	team, channel, ts := splitRef(p.Message.ExternalRef)
	if me, err := s.identity(ctx, core); err != nil {
		return plugin.MessageHookResult{}, err
	} else if team != me.TeamID || channel == "" || ts == "" || strings.Contains(ts, "/") {
		return plugin.MessageHookResult{}, nil
	}
	text, blocks := render(p.Message)
	if blocks == nil {
		blocks = []block{} // a card that became text loses its buttons
	}
	return plugin.MessageHookResult{ExternalRef: p.Message.ExternalRef}, s.client(core).Update(ctx, channel, ts, text, blocks)
}
