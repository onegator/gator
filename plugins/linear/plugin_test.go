package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/onegator/gator/plugin"
	"github.com/onegator/gator/plugin/plugintest"
)

const secret = "linear-signing-secret"

var built struct {
	once sync.Once
	path string
	err  error
}

func pluginBinary(t *testing.T) string {
	t.Helper()
	built.once.Do(func() {
		dir, err := os.MkdirTemp("", "gator-plugin-linear")
		if err != nil {
			built.err = err
			return
		}
		built.path = filepath.Join(dir, "linear")
		if out, err := exec.Command("go", "build", "-o", built.path, ".").CombinedOutput(); err != nil {
			built.err = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if built.err != nil {
		t.Fatal(built.err)
	}
	return built.path
}

// fakeLinear answers the two GraphQL calls the plugin makes and remembers the moves.
type fakeLinear struct {
	*httptest.Server
	mu    sync.Mutex
	moves []string // issueID→stateID
	keys  []string
}

func newFake(t *testing.T) *fakeLinear {
	f := &fakeLinear{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.keys = append(f.keys, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "lin_api_ok" {
			_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`))
			return
		}
		switch {
		case strings.Contains(in.Query, "states"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"team":{"states":{"nodes":[
				{"id":"st-todo","name":"Todo"},{"id":"st-prog","name":"In Progress"},{"id":"st-done","name":"Done"}]}}}}}`))
		case strings.Contains(in.Query, "issueUpdate"):
			f.moves = append(f.moves, fmt.Sprintf("%v→%v", in.Variables["id"], in.Variables["state"]))
			_, _ = w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		default:
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func start(t *testing.T, core *plugintest.Core, api string, cfg map[string]any, key string) *plugintest.Process {
	t.Helper()
	config := map[string]any{"api_url": api, "state_map": "implementation=In Progress, approved=Done"}
	for k, v := range cfg {
		config[k] = v
	}
	secrets := map[string]string{"webhook_secret": secret}
	if key != "" {
		secrets["api_key"] = key
	}
	p, err := plugintest.Start(context.Background(), []string{pluginBinary(t)}, core, plugintest.Options{Config: config, Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func sign(body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func deliver(p *plugintest.Process, payload any, signature string) error {
	body, _ := json.Marshal(payload)
	if signature == "" {
		signature = sign(body)
	}
	return p.Call(context.Background(), plugin.MethodWebhook, plugin.WebhookParams{
		Body: body, Headers: map[string]string{"Linear-Signature": signature}}, nil)
}

func issuePayload(action, identifier string, labels ...string) map[string]any {
	ls := []map[string]any{}
	for _, l := range labels {
		ls = append(ls, map[string]any{"name": l})
	}
	return map[string]any{"action": action, "type": "Issue", "data": map[string]any{
		"id": "uuid-" + identifier, "identifier": identifier, "title": "Search is slow",
		"description": "It takes ten seconds.", "url": "https://linear.app/acme/issue/" + identifier,
		"team": map[string]any{"key": "ENG"}, "labels": ls,
	}}
}

func TestANewIssueBecomesOneTask(t *testing.T) {
	core := plugintest.NewCore("")
	p := start(t, core, newFake(t).URL, nil, "lin_api_ok")
	for range 2 {
		if err := deliver(p, issuePayload("create", "ENG-12", "Bug"), ""); err != nil {
			t.Fatal(err)
		}
	}
	tasks := core.Tasks()
	if len(tasks) != 1 {
		t.Fatalf("one issue delivered twice should be one task, got %d", len(tasks))
	}
	if tasks[0].Kind != "bug" || tasks[0].ExternalRefs[refIssue] != "ENG-12" || tasks[0].ExternalRefs[refIssueID] != "uuid-ENG-12" {
		t.Fatalf("task = %+v", tasks[0])
	}
}

func TestOnlyLabelledIssuesAndTheRightTeamCount(t *testing.T) {
	core := plugintest.NewCore("")
	p := start(t, core, newFake(t).URL, map[string]any{"trigger_label": "gator", "team": "ENG"}, "")
	_ = deliver(p, issuePayload("create", "ENG-1"), "")
	if len(core.Tasks()) != 0 {
		t.Fatal("an issue without the trigger label should not become a task")
	}
	// Adding the label later is what brings an existing issue in.
	_ = deliver(p, issuePayload("update", "ENG-1", "gator"), "")
	if len(core.Tasks()) != 1 {
		t.Fatal("labelling an issue should bring it in")
	}
	other := issuePayload("create", "OPS-4", "gator")
	other["data"].(map[string]any)["team"] = map[string]any{"key": "OPS"}
	_ = deliver(p, other, "")
	if len(core.Tasks()) != 1 {
		t.Fatal("an issue from another team should be ignored")
	}
}

func TestAForgedDeliveryIsRefused(t *testing.T) {
	core := plugintest.NewCore("")
	p := start(t, core, newFake(t).URL, nil, "")
	if err := deliver(p, issuePayload("create", "ENG-2"), "deadbeef"); err == nil {
		t.Fatal("a bad signature should be refused")
	}
	if len(core.Tasks()) != 0 {
		t.Fatal("nothing should come of a forged delivery")
	}
}

// Gator owns the phase; Linear mirrors it, by the state names a person wrote in state_map.
func TestThePhaseIsMirroredInTheIssueState(t *testing.T) {
	api := newFake(t)
	core := plugintest.NewCore("")
	p := start(t, core, api.URL, nil, "lin_api_ok")
	task := plugin.TaskRef{ID: "t1", Phase: "implementation", ExternalRefs: map[string]string{refIssue: "ENG-9", refIssueID: "uuid-9"}}

	if err := p.Call(context.Background(), plugin.MethodPhaseTransition,
		plugin.PhaseTransitionParams{Task: task, From: "planning", To: "implementation"}, nil); err != nil {
		t.Fatal(err)
	}
	// A phase the map does not mention leaves the issue where it is.
	if err := p.Call(context.Background(), plugin.MethodPhaseTransition,
		plugin.PhaseTransitionParams{Task: task, From: "implementation", To: "verification"}, nil); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	moves, key := api.moves, api.keys[0]
	api.mu.Unlock()
	if len(moves) != 1 || moves[0] != "uuid-9→st-prog" {
		t.Fatalf("moves = %v", moves)
	}
	if key != "lin_api_ok" {
		t.Fatalf("a personal key goes in the header as it is, got %q", key)
	}

	var ui plugin.RenderUIResult
	if err := p.Call(context.Background(), plugin.MethodRenderUI, plugin.RenderUIParams{Task: task}, &ui); err != nil {
		t.Fatal(err)
	}
	if len(ui.Tabs) != 1 || !strings.Contains(ui.Tabs[0].Markdown, "ENG-9") || !strings.Contains(ui.Tabs[0].Markdown, "In Progress") {
		t.Fatalf("ui = %+v", ui)
	}
}

// A refused key is broken until somebody fixes it, so it must count against the plugin rather
// than be reported as a bad request nobody reads.
func TestARefusedKeyIsReportedAsTheCoreProblemItIs(t *testing.T) {
	core := plugintest.NewCore("")
	p := start(t, core, newFake(t).URL, nil, "lin_api_wrong")
	task := plugin.TaskRef{ID: "t1", ExternalRefs: map[string]string{refIssue: "ENG-9", refIssueID: "uuid-9"}}
	err := p.Call(context.Background(), plugin.MethodPhaseTransition,
		plugin.PhaseTransitionParams{Task: task, From: "planning", To: "implementation"}, nil)
	if err == nil || !strings.Contains(err.Error(), "refused the API key") {
		t.Fatalf("err = %v", err)
	}
}
