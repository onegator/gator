package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/onegator/gator/plugin"
)

// Comments are a conversation's way in: "@gator reviewer look at this" on a pull request or an
// issue asks the project's @reviewer, in the thread of the task the pull request belongs to, or
// as a new question in #github when it belongs to none. The answer comes back as a comment, short,
// with the way to the whole thread in Gator.

// replyMarker is in every comment the plugin writes, so that it never takes its own comment (made
// with whatever account's token it holds) for somebody asking.
const replyMarker = "<!-- gator:reply -->"

// githubChannel is where questions about no task are asked.
const githubChannel = "github"

// maxReply is how much of an answer a comment carries; the rest is in Gator.
const maxReply = 4000

// trigger finds "@gator <handle>" in a comment: the word after the mention is the agent asked.
func trigger(mention string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:^|\s)@` + regexp.QuoteMeta(mention) + `\s+@?([a-z0-9][a-z0-9-]{1,31})\b`)
}

// asked are the handles a comment asks, lowercased, once each.
func asked(body, mention string) []string {
	var out []string
	for _, m := range trigger(mention).FindAllStringSubmatch(body, -1) {
		h := strings.ToLower(m[1])
		if !contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

type comment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"` // User | Bot
	} `json:"user"`
}

// onComment takes a new comment on an issue or a pull request (issue_comment), or on a line of a
// pull request (pull_request_review_comment).
func (g *gh) onComment(ctx context.Context, core *plugin.Core, repo, event, action string, body []byte) error {
	if action != "created" {
		return nil
	}
	var ev struct {
		Comment comment `json:"comment"`
		Issue   *struct {
			Number      int       `json:"number"`
			Title       string    `json:"title"`
			PullRequest *struct{} `json:"pull_request"`
		} `json:"issue"`
		PullRequest *struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return plugin.Errorf(plugin.CodeInvalidParams, "comment: %v", err)
	}
	c := ev.Comment
	if c.ID == 0 || c.User.Type == "Bot" || strings.Contains(c.Body, replyMarker) {
		return nil // a bot's, or the plugin's own answer
	}
	handles := asked(c.Body, setting(core, "mention", "gator"))
	if len(handles) == 0 {
		return nil
	}
	number, title, isPR := 0, "", event == "pull_request_review_comment"
	switch {
	case ev.Issue != nil:
		number, title, isPR = ev.Issue.Number, ev.Issue.Title, ev.Issue.PullRequest != nil
	case ev.PullRequest != nil:
		number, title = ev.PullRequest.Number, ev.PullRequest.Title
	}
	if number == 0 {
		return nil
	}
	ref := refFor(repo, number)
	conv, err := g.conversationFor(ctx, core, repo, ref, isPR)
	if err != nil {
		return err
	}
	what := "issue"
	if isPR {
		what = "pull request"
	}
	text := fmt.Sprintf("On the %s %s (%s): %s\n\n%s", what, ref, title, c.HTMLURL, strings.TrimSpace(c.Body))
	_, err = core.PostMessage(ctx, plugin.MessagePostParams{ConversationID: conv, Body: text,
		Author:      plugin.ExternalAuthor{ExternalID: strconv.FormatInt(c.User.ID, 10), DisplayName: c.User.Login},
		ExternalRef: commentRef(ref, c.ID), Mentions: handles})
	return err
}

func commentRef(ref string, id int64) string { return fmt.Sprintf("%s/comment/%d", ref, id) }

// conversationFor is where a comment about ref is said: the thread of the task the pull request
// or issue belongs to, else #github.
func (g *gh) conversationFor(ctx context.Context, core *plugin.Core, repo, ref string, isPR bool) (string, error) {
	keys := []string{refIssue}
	if isPR {
		keys = []string{refPR, refReviewPR}
	}
	for _, k := range keys {
		t, err := core.FindTask(ctx, k, ref)
		if err != nil {
			return "", err
		}
		if t != nil {
			c, err := core.FindOrCreateConversation(ctx, plugin.ConversationFindOrCreateParams{ExternalRef: ref, Kind: plugin.BridgeTask, TaskID: t.ID})
			return c.ConversationID, err
		}
	}
	c, err := core.FindOrCreateConversation(ctx, plugin.ConversationFindOrCreateParams{ExternalRef: repo, Kind: plugin.BridgeChannel,
		Name: githubChannel, Topic: "Questions asked from " + repo + " on GitHub"})
	return c.ConversationID, err
}

// answered mirrors an agent's finished answer to a comment that asked it, as a comment on the same
// issue or pull request. Nothing else of the conversation goes to GitHub.
func (g *gh) answered(ctx context.Context, core *plugin.Core, p plugin.MessageHookParams) (plugin.MessageHookResult, error) {
	m := p.Message
	if m.AuthorKind != "agent" || m.Status != "final" || m.ReplyTo == "" || m.ExternalRef != "" || strings.TrimSpace(m.Body) == "" {
		return plugin.MessageHookResult{}, nil
	}
	ref, _, ok := strings.Cut(m.ReplyTo, "/comment/")
	if !ok {
		return plugin.MessageHookResult{}, nil
	}
	repo := core.Setting("repo")
	number, ok := refNumber(ref, repo)
	if !ok {
		return plugin.MessageHookResult{}, nil
	}
	body := strings.TrimSpace(m.Body)
	if r := []rune(body); len(r) > maxReply {
		body = string(r[:maxReply]) + "…"
	}
	text := fmt.Sprintf("%s\n\n— %s via Gator · the whole thread: `%s`\n%s", neutralize(body), m.AuthorName, m.URL, replyMarker)
	id, err := g.client(core).Comment(ctx, number, text)
	if err != nil {
		return plugin.MessageHookResult{}, asPluginErr(err)
	}
	return plugin.MessageHookResult{ExternalRef: commentRef(ref, id)}, nil
}

// neutralize keeps an agent's answer from pinging people or teams on GitHub and from closing
// issues by a keyword: @ and closing keywords followed by a reference are broken with a zero-width
// joiner, which reads the same.
var (
	atMention = regexp.MustCompile(`(^|[^\w` + "`" + `])@([A-Za-z0-9])`)
	closing   = regexp.MustCompile(`(?i)\b(close[sd]?|fix(e[sd])?|resolve[sd]?)(\s+#\d+)`)
)

func neutralize(s string) string {
	s = strings.ReplaceAll(s, replyMarker, "")
	s = atMention.ReplaceAllString(s, "$1@‍$2")
	return closing.ReplaceAllString(s, "$1‍$3")
}

// Comment adds a comment to an issue or a pull request and answers its id.
func (c *Client) Comment(ctx context.Context, number int, body string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", c.repo(), number), map[string]string{"body": body}, &out)
	return out.ID, err
}
