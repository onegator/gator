package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

// fakeLinear answers the GraphQL calls the plugin makes and remembers what it was asked to do.
type fakeLinear struct {
	*httptest.Server
	mu       sync.Mutex
	moves    []string // issueID→stateID
	keys     []string
	states   map[string]string // issue id → current state id
	issues   []apiIssue
	hooks    []Webhook
	secrets  map[string]string // hook id → secret
	requests []string
}

var teamStates = []State{{ID: "st-todo", Name: "Todo", Type: "unstarted", Position: 1}, {ID: "st-prog", Name: "In Progress", Type: "started", Position: 2},
	{ID: "st-shipped", Name: "Shipped", Type: "completed", Position: 4}, {ID: "st-done", Name: "Done", Type: "completed", Position: 3},
	{ID: "st-cancel", Name: "Canceled", Type: "canceled", Position: 5}}

func stateByID(id string) State {
	for _, s := range teamStates {
		if s.ID == id {
			return s
		}
	}
	return teamStates[0]
}

func newFake(t *testing.T) *fakeLinear {
	f := &fakeLinear{states: map[string]string{}, secrets: map[string]string{}}
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
		if a := r.Header.Get("Authorization"); a != "lin_api_ok" && a != "Bearer lin_oauth_ok" {
			_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`))
			return
		}
		reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		switch {
		case strings.Contains(in.Query, "webhookCreate"):
			f.requests = append(f.requests, "webhookCreate")
			input := in.Variables["input"].(map[string]any)
			id := fmt.Sprintf("wh-%d", len(f.hooks)+1)
			f.hooks = append(f.hooks, Webhook{ID: id, URL: input["url"].(string)})
			f.secrets[id] = input["secret"].(string)
			reply(map[string]any{"webhookCreate": map[string]any{"success": true}})
		case strings.Contains(in.Query, "webhookUpdate"):
			f.requests = append(f.requests, "webhookUpdate")
			f.secrets[in.Variables["id"].(string)] = in.Variables["input"].(map[string]any)["secret"].(string)
			reply(map[string]any{"webhookUpdate": map[string]any{"success": true}})
		case strings.Contains(in.Query, "webhooks"):
			reply(map[string]any{"webhooks": map[string]any{"nodes": f.hooks}})
		case strings.Contains(in.Query, "issues("):
			filter, _ := json.Marshal(in.Variables["filter"])
			f.requests = append(f.requests, "issues "+string(filter))
			nodes := f.issues
			after, _ := in.Variables["after"].(string)
			from := 0
			if after != "" {
				fmt.Sscanf(after, "c%d", &from)
			}
			to := min(from+100, len(nodes))
			page := map[string]any{"nodes": nodes[from:to], "pageInfo": map[string]any{"hasNextPage": to < len(nodes), "endCursor": fmt.Sprintf("c%d", to)}}
			reply(map[string]any{"issues": page})
		case strings.Contains(in.Query, "states"):
			cur := stateByID(f.states[in.Variables["id"].(string)])
			reply(map[string]any{"issue": map[string]any{"state": cur, "team": map[string]any{"states": map[string]any{"nodes": teamStates}}}})
		case strings.Contains(in.Query, "issueUpdate"):
			f.moves = append(f.moves, fmt.Sprintf("%v→%v", in.Variables["id"], in.Variables["state"]))
			f.states[in.Variables["id"].(string)] = in.Variables["state"].(string)
			reply(map[string]any{"issueUpdate": map[string]any{"success": true}})
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
	maps.Copy(config, cfg)
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

func withState(p map[string]any, typ, name string) map[string]any {
	p["data"].(map[string]any)["state"] = map[string]any{"type": typ, "name": name}
	return p
}

// An issue edited, finished or reopened in Linear says the same on the board; the state Gator
// itself set for the task's phase is an echo, and a state in between does not move the phase.
func TestTheTaskFollowsItsIssue(t *testing.T) {
	core := plugintest.NewCore("")
	p := start(t, core, newFake(t).URL, nil, "")
	if err := deliver(p, withState(issuePayload("create", "ENG-5"), "unstarted", "Todo"), ""); err != nil {
		t.Fatal(err)
	}
	edited := withState(issuePayload("update", "ENG-5"), "started", "In Progress")
	edited["data"].(map[string]any)["title"] = "Search is very slow"
	if err := deliver(p, edited, ""); err != nil {
		t.Fatal(err)
	}
	task := core.Tasks()[0]
	if task.Title != "Search is very slow" || task.Closed || task.Phase != "idea" {
		t.Fatalf("after an edit: %+v", task)
	}
	_ = deliver(p, withState(issuePayload("update", "ENG-5"), "canceled", "Canceled"), "")
	if !core.Tasks()[0].Closed {
		t.Fatal("a canceled issue should close its task")
	}
	_ = deliver(p, withState(issuePayload("update", "ENG-5"), "started", "In Progress"), "")
	if core.Tasks()[0].Closed {
		t.Fatal("an issue moved back should reopen its task")
	}

	// approved=Done in the state map: Done arriving while the task is in approved is Gator's
	// own move coming back, not Linear finishing it.
	seeded := core.AddTask(plugin.TaskRef{Kind: "feature", Title: "Search is slow", Phase: "approved",
		ExternalRefs: map[string]string{refIssue: "ENG-6", refIssueID: "uuid-ENG-6"}})
	_ = deliver(p, withState(issuePayload("update", "ENG-6"), "completed", "Done"), "")
	if got, _ := core.Task(seeded.ID); got.Closed {
		t.Fatal("the echo of Gator's own move closed the task")
	}
	// A finished issue that is not on the board yet is not work.
	_ = deliver(p, withState(issuePayload("create", "ENG-7"), "completed", "Done"), "")
	if len(core.Tasks()) != 2 {
		t.Fatalf("a finished issue became a task: %d tasks", len(core.Tasks()))
	}
}

// Import brings in the unfinished issues of the team, once. A token from Connect Linear is an
// OAuth token, which wants Bearer; a personal key does not.
func TestImportBringsUnfinishedIssuesInOnce(t *testing.T) {
	api := newFake(t)
	for n := 1; n <= 130; n++ {
		a := apiIssue{ID: fmt.Sprintf("uuid-%d", n), Identifier: fmt.Sprintf("ENG-%d", n), Title: fmt.Sprintf("Issue %d", n),
			URL: fmt.Sprintf("https://linear.app/acme/issue/ENG-%d", n), State: teamStates[0]}
		a.Team.Key = "ENG"
		api.issues = append(api.issues, a)
	}
	api.issues[0].Labels.Nodes = append(api.issues[0].Labels.Nodes, struct {
		Name string `json:"name"`
	}{"Bug"})
	core := plugintest.NewCore("")
	p := start(t, core, api.URL, map[string]any{"team": "ENG", "trigger_label": "gator"}, "lin_oauth_ok")
	var first, second plugin.ImportResult
	if err := p.Call(context.Background(), plugin.MethodImport, plugin.ImportParams{}, &first); err != nil {
		t.Fatal(err)
	}
	// The filter asks Linear for the trigger label, which the fake ignores; every issue
	// carries none, so the plugin's own check keeps the board empty.
	if first.Created != 0 || first.Existing != 0 {
		t.Fatalf("labelled import: %+v", first)
	}
	p = start(t, core, api.URL, map[string]any{"team": "ENG"}, "lin_oauth_ok")
	if err := p.Call(context.Background(), plugin.MethodImport, plugin.ImportParams{}, &first); err != nil {
		t.Fatal(err)
	}
	if err := p.Call(context.Background(), plugin.MethodImport, plugin.ImportParams{}, &second); err != nil {
		t.Fatal(err)
	}
	if first.Created != 130 || second.Existing != 130 || second.Created != 0 || len(core.Tasks()) != 130 || core.Tasks()[0].Kind != "bug" {
		t.Fatalf("import: %+v then %+v, %d tasks", first, second, len(core.Tasks()))
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.keys[0] != "Bearer lin_oauth_ok" {
		t.Fatalf("an OAuth token goes as Bearer, got %q", api.keys[0])
	}
	if !strings.Contains(api.requests[0], `"team":{"key":{"eqIgnoreCase":"ENG"}}`) || !strings.Contains(api.requests[0], `"nin":["completed","canceled"]`) ||
		!strings.Contains(api.requests[0], `"labels"`) {
		t.Fatalf("filter: %s", api.requests[0])
	}
}

// A finished task moves its issue to the team's first completed state, or to the one the state
// map names; an issue already finished is left alone.
func TestATaskClosedFinishesItsIssue(t *testing.T) {
	api := newFake(t)
	core := plugintest.NewCore("")
	p := start(t, core, api.URL, nil, "lin_api_ok")
	task := plugin.TaskRef{ID: "t1", ExternalRefs: map[string]string{refIssue: "ENG-9", refIssueID: "uuid-9"}}
	for range 2 {
		if err := p.Call(context.Background(), plugin.MethodTaskClosed, plugin.TaskClosedParams{Task: task}, nil); err != nil {
			t.Fatal(err)
		}
	}
	p = start(t, core, api.URL, map[string]any{"state_map": "closed=Shipped"}, "lin_api_ok")
	task.ExternalRefs[refIssueID] = "uuid-10"
	if err := p.Call(context.Background(), plugin.MethodTaskClosed, plugin.TaskClosedParams{Task: task}, nil); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if !slices.Equal(api.moves, []string{"uuid-9→st-done", "uuid-10→st-shipped"}) {
		t.Fatalf("moves: %v", api.moves)
	}
}

func TestThePluginRegistersItsWebhook(t *testing.T) {
	api := newFake(t)
	run := func(want string) {
		p, err := plugintest.Start(context.Background(), []string{pluginBinary(t)}, plugintest.NewCore(""), plugintest.Options{
			Config: map[string]any{"api_url": api.URL}, Secrets: map[string]string{"api_key": "lin_oauth_ok", "webhook_secret": "whsec"},
			WebhookURL: "https://gator.example/hooks/demo/linear"})
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			api.mu.Lock()
			done := slices.Contains(api.requests, want)
			api.mu.Unlock()
			if done {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("no %s", want)
	}
	run("webhookCreate")
	run("webhookUpdate")
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.hooks) != 1 || api.hooks[0].URL != "https://gator.example/hooks/demo/linear" || api.secrets["wh-1"] != "whsec" {
		t.Fatalf("hooks: %+v %v", api.hooks, api.secrets)
	}
}
