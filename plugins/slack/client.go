package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client is the slice of Slack's Web API the bridge uses, with the workspace's bot token.
type Client struct {
	Base  string // https://slack.com/api
	Token string
	HTTP  *http.Client
	Err   error // why Base cannot be used; every call answers it
}

// apiError is Slack's {"ok": false, "error": "…"}.
type apiError struct{ Method, Code string }

func (e *apiError) Error() string { return "slack " + e.Method + ": " + e.Code }

// maxAnswer bounds what is read of one answer.
const maxAnswer = 1 << 20

// call posts params as JSON to a Web API method and decodes the answer into out. Methods that read
// (auth.test, users.info) take a form instead: Slack reads JSON bodies only where it writes.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	if c.Err != nil {
		return c.Err
	}
	if c.Token == "" {
		return errors.New("slack is not connected: no bot token")
	}
	var body []byte
	contentType := "application/json; charset=utf-8"
	if form, ok := params.(url.Values); ok {
		body, contentType = []byte(form.Encode()), "application/x-www-form-urlencoded"
	} else {
		var err error
		if body, err = json.Marshal(params); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Base, "/")+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack %s: HTTP %d", method, resp.StatusCode)
	}
	var ok struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &ok); err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	if !ok.OK {
		return &apiError{Method: method, Code: ok.Error}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Identity is who the bot is: its workspace and its own user.
type Identity struct {
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
}

// AuthTest answers who the token is.
func (c *Client) AuthTest(ctx context.Context) (Identity, error) {
	var id Identity
	err := c.call(ctx, "auth.test", url.Values{}, &id)
	return id, err
}

// Post is chat.postMessage; it answers the new message's ts.
func (c *Client) Post(ctx context.Context, channel, threadTS, text string, blocks []block) (string, error) {
	params := map[string]any{"channel": channel, "text": text, "unfurl_links": false, "unfurl_media": false}
	if threadTS != "" {
		params["thread_ts"] = threadTS
	}
	if len(blocks) > 0 {
		params["blocks"] = blocks
	}
	var out struct {
		TS string `json:"ts"`
	}
	err := c.call(ctx, "chat.postMessage", params, &out)
	return out.TS, err
}

// Update is chat.update.
func (c *Client) Update(ctx context.Context, channel, ts, text string, blocks []block) error {
	params := map[string]any{"channel": channel, "ts": ts, "text": text}
	if blocks != nil {
		params["blocks"] = blocks
	}
	return c.call(ctx, "chat.update", params, nil)
}

// Ephemeral is chat.postEphemeral: a line only one person sees.
func (c *Client) Ephemeral(ctx context.Context, channel, user, text string) error {
	return c.call(ctx, "chat.postEphemeral", map[string]any{"channel": channel, "user": user, "text": text}, nil)
}

// UserName is the name a user shows in Slack.
func (c *Client) UserName(ctx context.Context, user string) (string, error) {
	var out struct {
		User struct {
			Name    string `json:"name"`
			Profile struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.call(ctx, "users.info", url.Values{"user": {user}}, &out); err != nil {
		return "", err
	}
	for _, n := range []string{out.User.Profile.DisplayName, out.User.Profile.RealName, out.User.Name} {
		if n != "" {
			return n, nil
		}
	}
	return user, nil
}
