package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/plugin"
	"github.com/onegator/gator/plugin/plugintest"
)

var built struct {
	once sync.Once
	path string
	err  error
}

func pluginBinary(t *testing.T) string {
	t.Helper()
	built.once.Do(func() {
		dir, err := os.MkdirTemp("", "gator-plugin-slack")
		if err != nil {
			built.err = err
			return
		}
		built.path = filepath.Join(dir, "slack")
		if out, err := exec.Command("go", "build", "-o", built.path, ".").CombinedOutput(); err != nil {
			built.err = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if built.err != nil {
		t.Fatal(built.err)
	}
	return built.path
}

// fakeSlack is the slice of the Web API the bridge uses, for workspace T1 whose bot is UBOT.
type fakeSlack struct {
	*httptest.Server
	mu    sync.Mutex
	calls []call
	next  int
}

type call struct {
	Method string
	Auth   string
	Params map[string]any
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{next: 100}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		body, _ := io.ReadAll(r.Body)
		params := map[string]any{}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			_ = json.Unmarshal(body, &params)
		} else {
			form, _ := url.ParseQuery(string(body))
			for k := range form {
				params[k] = form.Get(k)
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, call{Method: method, Auth: r.Header.Get("Authorization"), Params: params})
		f.next++
		ts := fmt.Sprintf("1700000000.%06d", f.next)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer xoxb-test" {
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
			return
		}
		switch method {
		case "auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team_id":"T1","user_id":"UBOT","bot_id":"B1"}`))
		case "users.info":
			_, _ = w.Write([]byte(`{"ok":true,"user":{"name":"ada","profile":{"display_name":"Ada"}}}`))
		case "chat.postMessage":
			_, _ = fmt.Fprintf(w, `{"ok":true,"ts":%q}`, ts)
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSlack) of(method string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

const secret = "slack-signing-secret"

func start(t *testing.T, api *fakeSlack) *plugintest.Process {
	t.Helper()
	p, err := plugintest.Start(context.Background(), []string{pluginBinary(t)}, plugintest.NewCore(""), plugintest.Options{
		ProjectSlug: "demo", Config: map[string]any{"channels": "C1=eng", "api_url": api.URL + "/api"},
		Secrets: map[string]string{"bot_token": "xoxb-test", "signing_secret": secret}, AllowPrivateOutbound: true, APIHosts: "127.0.0.1", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// deliver sends a request as Slack would, signed now unless ts says otherwise.
func deliver(t *testing.T, p *plugintest.Process, contentType string, body []byte, ts int64, key string) (plugin.WebhookResult, error) {
	t.Helper()
	stamp := strconv.FormatInt(ts, 10)
	h := map[string]string{"Content-Type": contentType, "X-Slack-Request-Timestamp": stamp, "X-Slack-Signature": plugintest.SignSlack(key, stamp, body)}
	var out plugin.WebhookResult
	err := p.Call(context.Background(), plugin.MethodWebhook, plugin.WebhookParams{DeliveryID: "d", Headers: h, Body: body}, &out)
	return out, err
}

func event(t *testing.T, team string, ev map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"type": "event_callback", "team_id": team, "event": ev})
	return b
}

func refused(err error) bool {
	pe, ok := errors.AsType[*plugin.Error](err)
	return ok && pe.Code == plugin.CodeForbidden
}

func TestEveryRequestIsSignedFreshAndForThisWorkspace(t *testing.T) {
	api := newFakeSlack(t)
	p := start(t, api)
	now := time.Now().Unix()
	challenge := []byte(`{"type":"url_verification","challenge":"c-123"}`)
	out, err := deliver(t, p, "application/json", challenge, now, secret)
	if err != nil || out.Body != `{"challenge":"c-123"}` {
		t.Fatalf("challenge: %+v %v", out, err)
	}
	if _, err := deliver(t, p, "application/json", challenge, now, "another-secret"); !refused(err) {
		t.Errorf("a bad signature: %v", err)
	}
	if _, err := deliver(t, p, "application/json", challenge, now-int64(6*time.Minute/time.Second), secret); !refused(err) {
		t.Errorf("a request six minutes old was taken: %v", err)
	}
	msg := map[string]any{"type": "message", "channel": "C1", "user": "U1", "text": "hi", "ts": "1.1"}
	if _, err := deliver(t, p, "application/json", event(t, "T2", msg), now, secret); !refused(err) {
		t.Errorf("another workspace's event: %v", err)
	}
	if len(p.Core.Messages()) != 0 {
		t.Error("something was posted from a refused request")
	}
}

func TestMessagesAndThreadsComeInAsTheirAuthors(t *testing.T) {
	api := newFakeSlack(t)
	p := start(t, api)
	now := time.Now().Unix()
	post := func(ev map[string]any) {
		t.Helper()
		if _, err := deliver(t, p, "application/json", event(t, "T1", ev), now, secret); err != nil {
			t.Fatal(err)
		}
	}
	post(map[string]any{"type": "message", "channel": "C1", "user": "U1", "ts": "1.1",
		"text": "<@UBOT> reviewer look at <https://github.com/acme/app/pull/12|#12> &lt;now&gt; <!channel>"})
	post(map[string]any{"type": "message", "channel": "C1", "user": "U2", "ts": "1.2", "thread_ts": "1.1", "text": "and @planner too"})
	post(map[string]any{"type": "message", "channel": "C1", "bot_id": "B1", "user": "UBOT", "ts": "1.3", "text": "our own echo"})
	post(map[string]any{"type": "message", "channel": "C9", "user": "U1", "ts": "1.4", "text": "not a bridged channel"})
	post(map[string]any{"type": "message", "subtype": "channel_join", "channel": "C1", "user": "U3", "ts": "1.5", "text": "joined"})
	post(map[string]any{"type": "message", "subtype": "message_changed", "channel": "C1",
		"message": map[string]any{"user": "U1", "ts": "1.1", "text": "<@UBOT> reviewer look at #13"}})

	convs := p.Core.Conversations()
	if len(convs) != 2 || convs[0].Kind != "channel" || convs[0].Name != "eng" || convs[0].ExternalRef != "T1/C1" ||
		convs[1].Kind != "thread" || convs[1].ParentRef != "T1/C1/1.1" {
		t.Fatalf("conversations %+v", convs)
	}
	msgs := p.Core.Messages()
	if len(msgs) != 2 {
		t.Fatalf("messages %+v", msgs)
	}
	if msgs[0].Author != "T1/U1" || msgs[0].ExternalRef != "T1/C1/1.1" || msgs[0].Mentions[0] != "reviewer" {
		t.Errorf("first %+v", msgs[0])
	}
	if msgs[0].Body != "reviewer look at #13" {
		t.Errorf("the edit did not land: %q", msgs[0].Body)
	}
	if msgs[1].ConversationID != convs[1].ID || len(msgs[1].Mentions) != 1 || msgs[1].Mentions[0] != "planner" {
		t.Errorf("thread reply %+v", msgs[1])
	}
}

func TestPlainKeepsWordsAndDropsMarkup(t *testing.T) {
	got := plain("<@UBOT> reviewer look at <https://github.com/acme/app/pull/12|#12> &lt;now&gt; <!channel> <@U2|bob> <#C1|eng>", "UBOT")
	if got != "reviewer look at #12 (https://github.com/acme/app/pull/12) <now>  @U2 #eng" {
		t.Errorf("%q", got)
	}
}

func TestLinkAndApproveFromSlack(t *testing.T) {
	api := newFakeSlack(t)
	p := start(t, api)
	now := time.Now().Unix()
	form := url.Values{"command": {"/gator"}, "text": {"link GTR-ABCD-EFGH-IJKL-MNOP"}, "team_id": {"T1"}, "user_id": {"U1"}, "channel_id": {"C1"}}
	out, err := deliver(t, p, "application/x-www-form-urlencoded", []byte(form.Encode()), now, secret)
	if err != nil || !strings.Contains(out.Body, "not one") {
		t.Fatalf("an unknown code: %+v %v", out, err)
	}
	p.Core.LinkCode("GTR-ABCD-EFGH-IJKL-MNOP", "user-1")
	out, err = deliver(t, p, "application/x-www-form-urlencoded", []byte(form.Encode()), now, secret)
	if err != nil || !strings.Contains(out.Body, "Linked") || !strings.Contains(out.Body, "ephemeral") {
		t.Fatalf("link: %+v %v", out, err)
	}
	payload, _ := json.Marshal(map[string]any{"type": "block_actions", "team": map[string]any{"id": "T1"}, "user": map[string]any{"id": "U1"},
		"channel": map[string]any{"id": "C1"}, "actions": []map[string]any{{"action_id": approveAction, "value": "msg-1"}}})
	press := url.Values{"payload": {string(payload)}}
	if _, err := deliver(t, p, "application/x-www-form-urlencoded", []byte(press.Encode()), now, secret); err != nil {
		t.Fatal(err)
	}
	acts := p.Core.CardActions()
	if len(acts) != 1 || acts[0].MessageID != "msg-1" || acts[0].Actor.ExternalID != "T1/U1" || acts[0].Action != "approve" {
		t.Errorf("card actions %+v", acts)
	}
	if e := api.of("chat.postEphemeral"); len(e) != 1 || e[0].Params["text"] != "Approved." || e[0].Params["user"] != "U1" {
		t.Errorf("the presser was not told: %+v", e)
	}
	other, _ := json.Marshal(map[string]any{"type": "block_actions", "team": map[string]any{"id": "T2"}, "user": map[string]any{"id": "U1"},
		"actions": []map[string]any{{"action_id": approveAction, "value": "msg-1"}}})
	if _, err := deliver(t, p, "application/x-www-form-urlencoded", []byte(url.Values{"payload": {string(other)}}.Encode()), now, secret); !refused(err) {
		t.Errorf("a press from another workspace: %v", err)
	}
}

func TestMirroringOutPingsNobodyAndStreamsIntoOneMessage(t *testing.T) {
	api := newFakeSlack(t)
	p := start(t, api)
	conv := plugin.BridgedConversation{ID: "c1", Kind: "thread", ParentRef: "T1/C1/1.1", RootRef: "T1/C1"}
	msg := plugin.BridgedMessage{ID: "m1", Kind: "text", Status: "streaming", AuthorKind: "agent", AuthorName: "@reviewer",
		Body: "<!channel> look <https://evil.example|here> & there"}
	var r plugin.MessageHookResult
	if err := p.Call(context.Background(), plugin.MethodMessagePosted, plugin.MessageHookParams{Conversation: conv, Message: msg}, &r); err != nil {
		t.Fatal(err)
	}
	posts := api.of("chat.postMessage")
	if len(posts) != 1 || posts[0].Params["thread_ts"] != "1.1" || posts[0].Params["channel"] != "C1" || posts[0].Params["unfurl_links"] != false {
		t.Fatalf("posts %+v", posts)
	}
	text := posts[0].Params["text"].(string)
	if strings.Contains(text, "<!channel>") || strings.Contains(text, "<https") || !strings.Contains(text, "&lt;!channel&gt;") || !strings.HasSuffix(text, "…") {
		t.Errorf("text %q", text)
	}
	if !strings.HasPrefix(r.ExternalRef, "T1/C1/1700000000.") {
		t.Fatalf("ref %q", r.ExternalRef)
	}
	msg.Status, msg.Body, msg.ExternalRef = "final", "done", r.ExternalRef
	if err := p.Call(context.Background(), plugin.MethodMessageEdited, plugin.MessageHookParams{Conversation: conv, Message: msg}, &r); err != nil {
		t.Fatal(err)
	}
	ups := api.of("chat.update")
	if len(ups) != 1 || ups[0].Params["ts"] != strings.TrimPrefix(msg.ExternalRef, "T1/C1/") || ups[0].Params["text"] != "*@reviewer*: done" {
		t.Errorf("updates %+v", ups)
	}
	// A card waiting on a person gets an Approve button carrying its message id.
	card := plugin.BridgedMessage{ID: "m2", Kind: "card", CardType: "decision", Status: "final", Body: "Waiting: approve the plan",
		Actions: []string{"approve"}, URL: "gator://conversations/c1?seq=3"}
	if err := p.Call(context.Background(), plugin.MethodMessagePosted, plugin.MessageHookParams{
		Conversation: plugin.BridgedConversation{ExternalRef: "T1/C1"}, Message: card}, &r); err != nil {
		t.Fatal(err)
	}
	blocks, _ := json.Marshal(api.of("chat.postMessage")[1].Params["blocks"])
	if !strings.Contains(string(blocks), `"action_id":"gator_approve"`) || !strings.Contains(string(blocks), `"value":"m2"`) {
		t.Errorf("blocks %s", blocks)
	}
	// Somewhere of another workspace is not posted to.
	r = plugin.MessageHookResult{}
	if err := p.Call(context.Background(), plugin.MethodMessagePosted, plugin.MessageHookParams{
		Conversation: plugin.BridgedConversation{ExternalRef: "T2/C7"}, Message: msg}, &r); err != nil || r.ExternalRef != "" {
		t.Errorf("another workspace's channel: %+v %v", r, err)
	}
	for _, c := range api.calls {
		if c.Auth != "Bearer xoxb-test" {
			t.Errorf("%s without the bot token", c.Method)
		}
	}
}

func TestVerify(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte("x")
	sig := plugintest.SignSlack("k", "1700000000", body)
	if !verify("k", "1700000000", sig, body, now) || verify("", "1700000000", sig, body, now) || verify("k", "1700000000", sig, []byte("y"), now) ||
		verify("k", "1700000400", plugintest.SignSlack("k", "1700000400", body), body, now) || verify("k", "abc", sig, body, now) {
		t.Error("verify")
	}
}

// The bot token goes only where the operator allows: Slack's API, or a host listed in
// GATOR_PLUGIN_API_HOSTS — and even a listed host on this machine's side of the network only where
// private addresses are allowed. A project admin's api_url alone sends it nowhere else.
func TestTheBotTokenGoesOnlyWhereTheOperatorAllows(t *testing.T) {
	api := newFakeSlack(t)
	for name, o := range map[string]plugintest.Options{
		"an admin's address alone":           {AllowPrivateOutbound: true},
		"a listed host on a private address": {APIHosts: "127.0.0.1"},
	} {
		o.ProjectSlug, o.Timeout = "demo", 10*time.Second
		o.Config = map[string]any{"channels": "C1=eng", "api_url": api.URL + "/api"}
		o.Secrets = map[string]string{"bot_token": "xoxb-test", "signing_secret": secret}
		p, err := plugintest.Start(context.Background(), []string{pluginBinary(t)}, plugintest.NewCore(""), o)
		if err != nil {
			t.Fatal(err)
		}
		err = p.Call(context.Background(), plugin.MethodMessagePosted, plugin.MessageHookParams{
			Conversation: plugin.BridgedConversation{ExternalRef: "T1/C1"}, Message: plugin.BridgedMessage{Kind: "text", Body: "hi"}}, nil)
		_ = p.Close()
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(api.of("auth.test")) != 0 {
		t.Error("the fake API was called")
	}
}
