package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"github.com/onegator/gator/internal/server/api/gen"
)

var slackBin struct {
	once sync.Once
	path string
	err  error
}

func slackPlugin(t *testing.T) string {
	t.Helper()
	slackBin.once.Do(func() {
		slackBin.path = filepath.Join(os.TempDir(), "gator-e2e-slack-plugin")
		if out, err := exec.Command("go", "build", "-o", slackBin.path, "../../plugins/slack").CombinedOutput(); err != nil {
			slackBin.err = fmt.Errorf("build slack: %v\n%s", err, out)
		}
	})
	if slackBin.err != nil {
		t.Fatal(slackBin.err)
	}
	return slackBin.path
}

// fakeSlackAPI is Slack's Web API for workspace T1, whose bot is UBOT; it remembers what it was asked.
type fakeSlackAPI struct {
	*httptest.Server
	mu    sync.Mutex
	posts []map[string]any
	next  int
}

func newFakeSlackAPI(t *testing.T) *fakeSlackAPI {
	f := &fakeSlackAPI{next: 500}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer xoxb-e2e" {
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/api/") {
		case "auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team_id":"T1","user_id":"UBOT"}`))
		case "users.info":
			_, _ = w.Write([]byte(`{"ok":true,"user":{"name":"ada","profile":{"display_name":"Ada"}}}`))
		case "chat.postMessage":
			var p map[string]any
			_ = json.Unmarshal(body, &p)
			f.mu.Lock()
			f.next++
			f.posts = append(f.posts, p)
			ts := fmt.Sprintf("1800000000.%06d", f.next)
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"ok":true,"ts":%q}`, ts)
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSlackAPI) posted() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.posts...)
}

const slackSecret = "e2e-signing-secret"

// slack sends a request to the project's Slack hook as Slack would: signed at ts with secret.
func (h *harness) slack(slug, contentType string, body []byte, ts int64, secret string, extra ...string) (int, string) {
	h.t.Helper()
	stamp := strconv.FormatInt(ts, 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + stamp + ":"))
	m.Write(body)
	req, _ := http.NewRequest("POST", h.srv.URL+"/hooks/"+slug+"/slack", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(m.Sum(nil)))
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

func slackEvent(team, user, ts, threadTS, text string) []byte {
	ev := map[string]any{"type": "message", "channel": "C1", "user": user, "ts": ts, "text": text}
	if threadTS != "" {
		ev["thread_ts"] = threadTS
	}
	b, _ := json.Marshal(map[string]any{"type": "event_callback", "team_id": team, "event": ev})
	return b
}

// "@Gator reviewer …" in a connected Slack channel is a message of an outside identity in the
// project's channel, asks @reviewer in chat, and the answer goes back into the Slack thread. What a
// person says in the channel in Gator is mirrored to Slack; what came from Slack is not sent back.
// A request signed with another secret, too old, for another workspace or played again does
// nothing; a linked account speaks as its person.
func TestSlackBridgeEndToEnd(t *testing.T) {
	h := newHarness(t)
	api := newFakeSlackAPI(t)
	if code := h.do("PUT", "/plugins/slack", gen.InstallPlugin{Command: []string{slackPlugin(t)}}, nil); code != 200 {
		t.Fatalf("install: %d", code)
	}
	slug := fmt.Sprintf("sl%d", time.Now().UnixNano())
	var p gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: slug, Name: "Slack"}, &p)
	t.Cleanup(func() {
		_, _ = h.pool.Exec(t.Context(), "UPDATE project_plugins SET enabled = false WHERE project_id = $1", p.Id.String())
	})
	// Secret settings go in with the rest and never come back.
	cfg := map[string]any{"channels": "C1=eng", "api_url": api.URL + "/api", "bot_token": "xoxb-e2e", "signing_secret": slackSecret}
	var pp gen.ProjectPlugin
	if code := h.do("PUT", "/projects/"+p.Id.String()+"/plugins/slack", gen.ProjectPluginSettings{Enabled: true, Config: &cfg}, &pp); code != 200 {
		t.Fatalf("enable: %d %+v", code, pp)
	}
	if pp.Config["bot_token"] != nil || pp.Config["signing_secret"] != nil {
		t.Fatalf("a secret came back: %+v", pp.Config)
	}
	h.until("the bridge runs", func() bool {
		var vs []gen.ProjectPlugin
		h.do("GET", "/projects/"+p.Id.String()+"/plugins", nil, &vs)
		return len(vs) == 1 && vs[0].Running
	})
	now := time.Now().Unix()
	ask := slackEvent("T1", "U1", "1700000000.000100", "", "<@UBOT> reviewer is the plan for search safe?")
	if code, out := h.slack(slug, "application/json", ask, now, "forged"); code != 400 {
		t.Fatalf("a forged signature: %d %s", code, out)
	}
	if code, out := h.slack(slug, "application/json", ask, now-600, slackSecret); code != 400 {
		t.Fatalf("a ten-minute-old request: %d %s", code, out)
	}
	if code, out := h.slack(slug, "application/json", slackEvent("T2", "U1", "1.1", "", "<@UBOT> reviewer hi"), now, slackSecret); code != 400 {
		t.Fatalf("another workspace: %d %s", code, out)
	}
	if code, out := h.slack(slug, "application/json", ask, now, slackSecret); code != 200 || !strings.Contains(out, `"ok"`) {
		t.Fatalf("the ask: %d %s", code, out)
	}
	if code, out := h.slack(slug, "application/json", ask, now, slackSecret); code != 200 || !strings.Contains(out, "duplicate") {
		t.Fatalf("the same request again: %d %s", code, out)
	}
	// A header the signature does not cover does not make the replay a new delivery.
	if code, out := h.slack(slug, "application/json", ask, now, slackSecret, "X-Gator-Delivery", "fresh-1"); code != 200 || !strings.Contains(out, "duplicate") {
		t.Fatalf("a replay with a new delivery id: %d %s", code, out)
	}
	eng := ""
	_ = h.pool.QueryRow(t.Context(), `SELECT id::text FROM conversations WHERE project_id = $1 AND kind = 'channel' AND name = 'eng'`, p.Id.String()).Scan(&eng)
	if eng == "" {
		t.Fatal("no #eng")
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE conversation_id = $1 AND author_kind = 'external'`, eng); n != 1 {
		t.Fatalf("%d outside messages", n)
	}
	h.until("@reviewer is asked from outside, in chat", func() bool {
		return h.count(`SELECT count(*) FROM agent_turns t JOIN agents a ON a.id = t.agent_id
			WHERE t.project_id = $1 AND a.handle = 'reviewer' AND t.requested_by_kind = 'external' AND t.tier = 'chat'`, p.Id.String()) == 1
	})
	h.until("the answer goes into the Slack thread", func() bool {
		for _, m := range api.posted() {
			if m["thread_ts"] == "1700000000.000100" && strings.Contains(fmt.Sprint(m["text"]), "@reviewer") {
				return true
			}
		}
		return false
	})
	// A person in Gator says something in #eng: Slack gets it, and its echo is dropped.
	var said gen.Message
	if code := h.do("POST", "/conversations/"+eng+"/messages", gen.NewMessage{Body: new("<!channel> deploying now")}, &said); code != 201 {
		t.Fatalf("post: %d", code)
	}
	var mirrored map[string]any
	h.until("the person's message is mirrored", func() bool {
		for _, m := range api.posted() {
			if strings.Contains(fmt.Sprint(m["text"]), "deploying now") {
				mirrored = m
				return true
			}
		}
		return false
	})
	if strings.Contains(fmt.Sprint(mirrored["text"]), "<!channel>") || mirrored["thread_ts"] != nil {
		t.Errorf("mirrored %v", mirrored)
	}
	for _, m := range api.posted() {
		if strings.Contains(fmt.Sprint(m["text"]), "is the plan for search safe") {
			t.Error("the Slack message was echoed back to Slack")
		}
	}
	// Linking: a code made in Gator, typed in Slack.
	var code gen.ExternalLinkCode
	if c := h.do("POST", "/projects/"+p.Id.String()+"/external-links", gen.NewExternalLink{Provider: "slack"}, &code); c != 201 {
		t.Fatalf("code: %d", c)
	}
	form := url.Values{"command": {"/gator"}, "text": {"link " + code.Code}, "team_id": {"T1"}, "user_id": {"U1"}, "channel_id": {"C1"}}
	if c, out := h.slack(slug, "application/x-www-form-urlencoded", []byte(form.Encode()), now, slackSecret); c != 200 || !strings.Contains(out, "Linked") {
		t.Fatalf("link: %d %s", c, out)
	}
	if c, _ := h.slack(slug, "application/json", slackEvent("T1", "U1", "1700000000.000200", "", "now it is me"), now, slackSecret); c != 200 {
		t.Fatal(c)
	}
	if n := h.count(`SELECT count(*) FROM messages WHERE conversation_id = $1 AND author_kind = 'user' AND body = 'now it is me'`, eng); n != 1 {
		t.Error("a linked account does not speak as its person")
	}
	var ids []gen.ExternalIdentity
	h.do("GET", "/projects/"+p.Id.String()+"/external-identities", nil, &ids)
	if len(ids) != 1 || !ids[0].Linked || ids[0].ExternalId != "T1/U1" {
		t.Errorf("identities %+v", ids)
	}
	// Nothing of the bot token is in what the plugin's calls recorded.
	if n := h.count(`SELECT count(*) FROM plugin_calls c JOIN project_plugins pp ON pp.id = c.project_plugin_id
		WHERE pp.project_id = $1 AND (c.payload::text LIKE '%xoxb-e2e%' OR c.result::text LIKE '%xoxb-e2e%')`, p.Id.String()); n != 0 {
		t.Error("the bot token is in the plugin's calls")
	}
}
