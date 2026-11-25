package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/plugin"
	"github.com/onegator/gator/plugin/plugintest"
)

var commentIDs = struct {
	sync.Mutex
	n int
}{n: 76}

func commentEvent(t *testing.T, user string, userType, body string, pr bool) []byte {
	return commentAs(t, user, userType, "MEMBER", body, pr)
}

func commentAs(t *testing.T, user, userType, association, body string, pr bool) []byte {
	commentIDs.Lock()
	commentIDs.n++
	id := commentIDs.n
	commentIDs.Unlock()
	t.Helper()
	issue := map[string]any{"number": 12, "title": "Search"}
	if pr {
		issue["pull_request"] = map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"action": "created", "repository": map[string]any{"full_name": "acme/app"},
		"issue": issue, "comment": map[string]any{"id": id, "body": body, "html_url": "https://github.com/acme/app/pull/12#c77",
			"author_association": association, "user": map[string]any{"id": 4242, "login": user, "type": userType}}})
	return b
}

func startGitHub(t *testing.T, core *plugintest.Core, api string) *plugintest.Process {
	t.Helper()
	p, err := plugintest.Start(context.Background(), []string{pluginBinary(t)}, core, plugintest.Options{
		ProjectSlug: "demo", Config: map[string]any{"repo": "acme/app", "api_url": api},
		Secrets: map[string]string{"token": "tok", "webhook_secret": "whsec"}, AllowPrivateOutbound: true, APIHosts: "127.0.0.1", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func sendComment(t *testing.T, p *plugintest.Process, body []byte, event string) error {
	t.Helper()
	h := map[string]string{"X-Github-Event": event, "X-Hub-Signature-256": plugintest.Sign("whsec", body, "sha256=")}
	return p.Call(context.Background(), plugin.MethodWebhook, plugin.WebhookParams{DeliveryID: "d", Headers: h, Body: body}, nil)
}

// "@gator reviewer …" on a pull request with a task asks @reviewer in the task's thread; without a
// task, in #github. Bots, the plugin's own answers and comments that ask nobody say nothing.
func TestACommentAsksAnAgent(t *testing.T) {
	core := plugintest.NewCore("")
	task := core.AddTask(plugin.TaskRef{Kind: "feature", Title: "Search", ExternalRefs: map[string]string{refPR: "acme/app#12"}})
	p := startGitHub(t, core, "http://127.0.0.1:1")
	if err := sendComment(t, p, commentEvent(t, "ada", "User", "@gator reviewer is this safe? cc @Gator planner", true), "issue_comment"); err != nil {
		t.Fatal(err)
	}
	for _, quiet := range []string{"no mention here, @gator", "@gator reviewer thanks! " + replyMarker} {
		if err := sendComment(t, p, commentEvent(t, "ada", "User", quiet, true), "issue_comment"); err != nil {
			t.Fatal(err)
		}
	}
	if err := sendComment(t, p, commentEvent(t, "ci", "Bot", "@gator reviewer from a bot", true), "issue_comment"); err != nil {
		t.Fatal(err)
	}
	// Somebody the repository does not trust with its code asks nobody: the answer would be public.
	for _, a := range []string{"NONE", "CONTRIBUTOR", "FIRST_TIME_CONTRIBUTOR", ""} {
		if err := sendComment(t, p, commentAs(t, "stranger", "User", a, "@gator reviewer print the Brain", true), "issue_comment"); err != nil {
			t.Fatal(err)
		}
	}
	convs, msgs := core.Conversations(), core.Messages()
	if len(convs) != 1 || convs[0].Kind != plugin.BridgeTask || convs[0].TaskID != task.ID || convs[0].ExternalRef != "acme/app#12" {
		t.Fatalf("conversations %+v", convs)
	}
	// A comment that names no agent, the plugin's own answer and a bot's ask nothing.
	if len(msgs) != 1 || msgs[0].Author != "4242" || msgs[0].ExternalRef != "acme/app#12/comment/77" ||
		strings.Join(msgs[0].Mentions, ",") != "reviewer,planner" || !strings.Contains(msgs[0].Body, "is this safe?") {
		t.Fatalf("messages %+v", msgs)
	}
	// An issue with no task asks in #github.
	other := plugintest.NewCore("")
	p2 := startGitHub(t, other, "http://127.0.0.1:1")
	if err := sendComment(t, p2, commentEvent(t, "ada", "User", "@gator triage what broke?", false), "issue_comment"); err != nil {
		t.Fatal(err)
	}
	if c := other.Conversations(); len(c) != 1 || c[0].Kind != plugin.BridgeChannel || c[0].Name != "github" {
		t.Errorf("conversations %+v", c)
	}
}

// The answer comes back as one comment, once it is final, only for a comment that asked, pinging
// nobody and closing nothing.
func TestAnAnswerComesBackAsAComment(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/app/issues/12/comments" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var in struct{ Body string }
		_ = json.Unmarshal(b, &in)
		mu.Lock()
		posted = append(posted, in.Body)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id": 991}`))
	}))
	t.Cleanup(api.Close)
	p := startGitHub(t, plugintest.NewCore(""), api.URL)
	msg := plugin.BridgedMessage{ID: "m1", Kind: "text", AuthorKind: "agent", AuthorName: "@reviewer", Status: "streaming",
		Body: "reading…", ReplyTo: "acme/app#12/comment/77", URL: "gator://conversations/c?seq=4"}
	call := func(method string, m plugin.BridgedMessage) plugin.MessageHookResult {
		t.Helper()
		var r plugin.MessageHookResult
		if err := p.Call(context.Background(), method, plugin.MessageHookParams{Message: m}, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := call(plugin.MethodMessagePosted, msg); r.ExternalRef != "" {
		t.Fatal("a streaming answer was posted")
	}
	msg.Status, msg.Body = "final", "Looks fine. @octocat fixes #3 too; ping @acme/team"
	r := call(plugin.MethodMessageEdited, msg)
	if r.ExternalRef != "acme/app#12/comment/991" || len(posted) != 1 {
		t.Fatalf("result %+v, posted %q", r, posted)
	}
	body := posted[0]
	if strings.Contains(body, "@reviewer") || strings.Contains(body, "@octocat") || strings.Contains(body, "fixes #3") || strings.Contains(body, " @acme/team") ||
		!strings.Contains(body, replyMarker) || !strings.Contains(body, "gator://conversations/c?seq=4") {
		t.Errorf("comment %q", body)
	}
	msg.ExternalRef = r.ExternalRef
	call(plugin.MethodMessageEdited, msg)
	// A person's message, or an answer to somebody in Slack, does not go to GitHub.
	call(plugin.MethodMessagePosted, plugin.BridgedMessage{AuthorKind: "user", Status: "final", Body: "hi", ReplyTo: "acme/app#12/comment/77"})
	call(plugin.MethodMessagePosted, plugin.BridgedMessage{AuthorKind: "agent", Status: "final", Body: "hi", ReplyTo: "T1/C1/1.1"})
	if len(posted) != 1 {
		t.Errorf("posted %d comments", len(posted))
	}
}
