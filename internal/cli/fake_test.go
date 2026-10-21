package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/onegator/gator/internal/server/auth"
)

// fakeGator answers the calls the CLI makes, from memory, and checks every /api/v1 request
// against docs/openapi.yaml: the hand-written client is held to the same contract as the
// server.
type fakeGator struct {
	t      *testing.T
	srv    *httptest.Server
	router routers.Router

	mu        sync.Mutex
	token     string
	revoked   bool
	projects  []Project
	tasks     map[string][]Task
	jobs      map[string][]Job
	events    map[string][]JobEvent
	newJobs   []map[string]any
	pending   map[string]string // login code → S256 challenge
	redirects map[string]string // login code → redirect_uri

	// Knobs for one test at a time.
	noOIDC          bool
	loginError      string // the provider refused: sent back as ?error=
	rejectPlacement bool   // a server from before PLQ-283
	noWebSocket     bool
	// onSubscribe runs when a WebSocket client subscribes; it may push messages.
	onSubscribe func(send func(typ string, payload any))
	// pollsUntilDone finishes the running job after this many reads of it.
	pollsUntilDone int
}

const (
	projID = "11111111-1111-4111-8111-111111111111"
	taskID = "22222222-2222-4222-8222-222222222222"
	jobID  = "33333333-3333-4333-8333-333333333333"
	userID = "44444444-4444-4444-8444-444444444444"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func newFake(t *testing.T) *fakeGator {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Paths are matched without the /api/v1 prefix the spec's server carries.
	doc.Servers = nil
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGator{
		t: t, router: router, token: "gtr_user_test",
		projects: []Project{
			{ID: projID, Slug: "shop", Name: "Shop", Tags: []string{}, CreatedAt: t0, Role: "admin"},
			{ID: "55555555-5555-4555-8555-555555555555", Slug: "old", Name: "Old site", Tags: []string{}, CreatedAt: t0, Role: "viewer", ArchivedAt: &t0},
		},
		tasks: map[string][]Task{projID: {
			{ID: taskID, ProjectID: projID, Kind: "feature", Title: "Checkout with Apple Pay", Phase: "implement", Urgency: 2, OwnerKind: "user", OwnerID: userID, PhaseEnteredAt: t0, CreatedAt: t0},
			{ID: "66666666-6666-4666-8666-666666666666", ProjectID: projID, Kind: "bug", Title: "Cart forgets items", Phase: "triage", Urgency: 1, BlockedReason: "which browser?", PhaseEnteredAt: t0, CreatedAt: t0},
			{ID: "77777777-7777-4777-8777-777777777777", ProjectID: projID, Kind: "chore", Title: "Bump Go", Phase: "done", Urgency: 3, PhaseEnteredAt: t0, CreatedAt: t0, ClosedAt: &t0},
		}},
		jobs:      map[string][]Job{},
		events:    map[string][]JobEvent{},
		pending:   map[string]string{},
		redirects: map[string]string{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// env is a CLI environment pointed at the fake, with its config in a temporary directory and
// its token in a file store there.
func (f *fakeGator) env(t *testing.T) (*Env, *bytes.Buffer, *bytes.Buffer) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	vars := map[string]string{}
	e := &Env{
		Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		Getenv: func(k string) string { return vars[k] }, ConfigDir: dir,
		Store: FileStore{Path: filepath.Join(dir, "credentials.json")}, HTTP: f.srv.Client(),
		Hostname: "testbox", LoginTimeout: 5 * time.Second, PollInterval: 10 * time.Millisecond,
	}
	return e, &stdout, &stderr
}

// signIn stores the fake's token as if gator login had run.
func (f *fakeGator) signIn(t *testing.T, e *Env) {
	t.Helper()
	if err := e.Store.Set(f.srv.URL, f.token); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(e.ConfigDir, Config{Server: f.srv.URL}); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeGator) validate(r *http.Request, body []byte) {
	req := r.Clone(r.Context())
	req.URL = &url.URL{Path: strings.TrimPrefix(r.URL.Path, "/api/v1"), RawQuery: r.URL.RawQuery}
	req.Body = io.NopCloser(bytes.NewReader(body))
	route, params, err := f.router.FindRoute(req)
	if err != nil {
		f.t.Errorf("%s %s is not in docs/openapi.yaml: %v", r.Method, r.URL.Path, err)
		return
	}
	err = openapi3filter.ValidateRequest(r.Context(), &openapi3filter.RequestValidationInput{
		Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	})
	if err != nil {
		f.t.Errorf("%s %s does not match docs/openapi.yaml: %v", r.Method, r.URL.Path, err)
	}
}

func writeJSONT(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, status int, msg string) {
	writeJSONT(w, status, map[string]string{"error": msg, "code": "x"})
}

func (f *fakeGator) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path

	// Sign-in lives outside /api/v1 and outside the contract; it mirrors internal/server/auth.
	switch path {
	case "/auth/config":
		if f.noOIDC {
			writeJSONT(w, 200, map[string]any{"oidc": false})
			return
		}
		writeJSONT(w, 200, map[string]any{"oidc": true, "provider": "Google"})
		return
	case "/auth/login":
		f.login(w, r)
		return
	case "/auth/token":
		f.exchange(w, body)
		return
	case "/auth/logout":
		f.mu.Lock()
		if r.Header.Get("Authorization") == "Bearer "+f.token {
			f.revoked = true
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !strings.HasPrefix(path, "/api/v1/") {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	authorized := !f.revoked && r.Header.Get("Authorization") == "Bearer "+f.token
	f.mu.Unlock()
	if !authorized {
		problem(w, 401, "invalid token")
		return
	}
	if path == "/api/v1/ws" {
		f.websocket(w, r)
		return
	}
	f.validate(r, body)

	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/"), "/")
	switch {
	case path == "/api/v1/auth/me":
		writeJSONT(w, 200, Me{Kind: "user", UserID: userID, Name: "Ada Lovelace", WorkspaceRole: "member", Scope: "user"})
	case path == "/api/v1/projects" && r.Method == http.MethodGet:
		writeJSONT(w, 200, f.projects)
	case path == "/api/v1/projects" && r.Method == http.MethodPost:
		var in NewProject
		_ = json.Unmarshal(body, &in)
		for _, p := range f.projects {
			if p.Slug == in.Slug {
				problem(w, 409, fmt.Sprintf("a project called %q already exists; pick another short name", in.Slug))
				return
			}
		}
		p := Project{ID: "88888888-8888-4888-8888-888888888888", Slug: in.Slug, Name: in.Name, Tags: []string{}, CreatedAt: t0, Role: "admin"}
		f.projects = append(f.projects, p)
		writeJSONT(w, 201, p)
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "tasks" && r.Method == http.MethodGet:
		writeJSONT(w, 200, append([]Task{}, f.tasks[parts[1]]...))
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "tasks" && r.Method == http.MethodPost:
		var in NewTask
		_ = json.Unmarshal(body, &in)
		t := Task{ID: "99999999-9999-4999-8999-999999999999", ProjectID: parts[1], Kind: in.Kind, Title: in.Title, Description: in.Description, Phase: "idea", Urgency: 3, PhaseEnteredAt: t0, CreatedAt: t0}
		f.tasks[parts[1]] = append(f.tasks[parts[1]], t)
		writeJSONT(w, 201, t)
	case len(parts) == 2 && parts[0] == "tasks":
		t, ok := f.task(parts[1])
		if !ok {
			problem(w, 404, "not found")
			return
		}
		writeJSONT(w, 200, TaskDetail{Task: t, Phases: []string{"idea", "plan", "implement", "review", "done"}, Gate: Gate{
			Phase: t.Phase, Kind: "both", Checks: []Check{{Name: "tests", Source: "ci", Status: "pass"}, {Name: "review", Source: "agent", Status: "pending", Detail: "waiting"}},
		}})
	case len(parts) == 3 && parts[0] == "tasks" && parts[2] == "advance":
		t, _ := f.task(parts[1])
		t.Phase = "review"
		writeJSONT(w, 200, t)
	case len(parts) == 3 && parts[0] == "tasks" && parts[2] == "approve":
		problem(w, 409, "gate not satisfied: review is pending")
	case len(parts) == 3 && parts[0] == "tasks" && parts[2] == "jobs" && r.Method == http.MethodGet:
		if _, ok := f.task(parts[1]); !ok {
			problem(w, 404, "not found")
			return
		}
		writeJSONT(w, 200, append([]Job{}, f.jobs[parts[1]]...))
	case len(parts) == 3 && parts[0] == "tasks" && parts[2] == "jobs" && r.Method == http.MethodPost:
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		if _, ok := in["placement"]; ok && f.rejectPlacement {
			problem(w, 400, `invalid body: json: unknown field "placement"`)
			return
		}
		f.newJobs = append(f.newJobs, in)
		j := Job{ID: jobID, TaskID: parts[1], ProjectID: projID, Phase: "implement", Role: "developer", Backend: "claude", Status: "queued", MaxAttempts: 3, CreatedAt: t0}
		if p, ok := in["placement"].(string); ok {
			j.Placement = p
		}
		f.jobs[parts[1]] = append(f.jobs[parts[1]], j)
		writeJSONT(w, 201, j)
	case len(parts) == 2 && parts[0] == "jobs":
		j, ok := f.job(parts[1])
		if !ok {
			problem(w, 404, "not found")
			return
		}
		if f.pollsUntilDone > 0 {
			f.pollsUntilDone--
			if f.pollsUntilDone == 0 {
				f.finishJob(parts[1], "done")
				j, _ = f.job(parts[1])
			}
		}
		writeJSONT(w, 200, j)
	case len(parts) == 3 && parts[0] == "jobs" && parts[2] == "events":
		var after int64
		fmt.Sscan(r.URL.Query().Get("after"), &after)
		out := []JobEvent{}
		for _, e := range f.events[parts[1]] {
			if e.Seq > after {
				out = append(out, e)
			}
		}
		writeJSONT(w, 200, out)
	default:
		problem(w, 404, "the fake does not know "+r.Method+" "+path)
	}
}

func (f *fakeGator) task(id string) (Task, bool) {
	for _, ts := range f.tasks {
		for _, t := range ts {
			if t.ID == id {
				return t, true
			}
		}
	}
	return Task{}, false
}

func (f *fakeGator) job(id string) (Job, bool) {
	for _, js := range f.jobs {
		for _, j := range js {
			if j.ID == id {
				return j, true
			}
		}
	}
	return Job{}, false
}

// finishJob must be called with mu held.
func (f *fakeGator) finishJob(id, status string) {
	for task, js := range f.jobs {
		for i := range js {
			if js[i].ID == id {
				f.jobs[task][i].Status = status
			}
		}
	}
}

// addEvents stores output for a job, as a runner's report would.
func (f *fakeGator) addEvents(jobID string, events ...JobEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[jobID] = append(f.events[jobID], events...)
}

// login stands in for /auth/login, the provider and /auth/callback at once: it checks what a
// native client must send and sends the browser straight back with a code.
func (f *fakeGator) login(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if !auth.NativeRedirectAllowed(redirect) || !strings.HasPrefix(redirect, "http://127.0.0.1:") {
		http.Error(w, "redirect_uri is not a native client address", 400)
		return
	}
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) < 43 || q.Get("state") == "" {
		http.Error(w, "a native client must send state and an S256 code_challenge", 400)
		return
	}
	back, _ := url.Parse(redirect)
	v := url.Values{"state": {q.Get("state")}}
	if f.loginError != "" {
		v.Set("error", f.loginError)
	} else {
		code := "code-" + q.Get("state")[:8]
		f.mu.Lock()
		f.pending[code] = q.Get("code_challenge")
		f.redirects[code] = redirect
		f.mu.Unlock()
		v.Set("code", code)
	}
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (f *fakeGator) exchange(w http.ResponseWriter, body []byte) {
	var in tokenRequest
	_ = json.Unmarshal(body, &in)
	f.mu.Lock()
	challenge, ok := f.pending[in.Code]
	redirect := f.redirects[in.Code]
	delete(f.pending, in.Code)
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(in.CodeVerifier))
	switch {
	case !ok:
		problem(w, 400, "the login code is unknown or already used")
	case base64.RawURLEncoding.EncodeToString(sum[:]) != challenge:
		problem(w, 400, "code_verifier does not match the sign-in")
	case in.RedirectURI != redirect:
		problem(w, 400, "redirect_uri does not match the sign-in")
	case in.ClientName != "gator CLI on testbox":
		problem(w, 400, "unexpected client_name "+in.ClientName)
	default:
		writeJSONT(w, 200, tokenResponse{Token: f.token, ExpiresAt: t0.Add(90 * 24 * time.Hour)})
	}
}

func (f *fakeGator) websocket(w http.ResponseWriter, r *http.Request) {
	if f.noWebSocket {
		http.NotFound(w, r)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, first, err := c.Read(ctx)
	if err != nil {
		return
	}
	var sub struct {
		Subscribe []string `json:"subscribe"`
	}
	if json.Unmarshal(first, &sub) != nil || len(sub.Subscribe) != 1 || sub.Subscribe[0] != "job:"+jobID {
		_ = c.Close(websocket.StatusPolicyViolation, "unexpected subscription")
		return
	}
	if f.onSubscribe != nil {
		f.onSubscribe(func(typ string, payload any) {
			p, _ := json.Marshal(payload)
			msg, _ := json.Marshal(map[string]any{"id": 1, "type": typ, "aggregate": "job", "aggregateId": jobID, "payload": json.RawMessage(p), "createdAt": t0})
			_ = c.Write(ctx, websocket.MessageText, msg)
		})
	}
	// Hold the connection until the client goes.
	_, _, _ = c.Read(ctx)
}
