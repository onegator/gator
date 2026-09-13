package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store"
)

type harness struct {
	t     *testing.T
	srv   *httptest.Server
	user  string
	relay *events.Relay
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	url := os.Getenv("GATOR_DATABASE_URL")
	if url == "" {
		t.Skip("GATOR_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var uid string
	if err := pool.QueryRow(ctx, "INSERT INTO users(email,name) VALUES($1,'t') RETURNING id::text", fmt.Sprintf("api-%d@t.local", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	cat, _ := process.DefaultCatalog()
	svc := process.NewService(pool, process.DBTemplates{Pool: pool, Defaults: cat}, process.StaticCapabilities(nil))
	hub := events.NewHub()
	relay := &events.Relay{Pool: pool, Hub: hub, Interval: 50 * time.Millisecond, Log: slog.Default()}
	rctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go relay.Run(rctx)
	s := &Server{Pool: pool, Process: svc, Hub: hub, Features: []string{"inbox"}, Log: slog.Default()}
	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, user: uid, relay: relay}
}

func (h *harness) do(method, path string, body any, out any) int {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+"/api/v1"+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gator-User", h.user)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func TestTaskLifecycleOverHTTPWithLiveEvents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var project gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("p%d", time.Now().UnixNano()), Name: "P"}, &project); code != 201 {
		t.Fatalf("create project: %d", code)
	}

	// subscribe before acting so we see the events
	ws, _, err := websocket.Dial(ctx, strings.Replace(h.srv.URL, "http", "ws", 1)+"/api/v1/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"subscribe":["inbox"]}`)); err != nil {
		t.Fatal(err)
	}

	var task gen.Task
	if code := h.do("POST", "/projects/"+project.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "Crash on login"}, &task); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	if task.Phase != "planning" {
		t.Fatalf("bug starts in %q", task.Phase)
	}

	readEvent := func(want string) {
		t.Helper()
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		for {
			_, b, err := ws.Read(rctx)
			if err != nil {
				t.Fatalf("waiting for %s: %v", want, err)
			}
			var e events.Event
			_ = json.Unmarshal(b, &e)
			if e.Type == want {
				return
			}
		}
	}
	readEvent("task.created")

	var inbox []gen.Decision
	h.do("GET", "/inbox?projectId="+project.Id.String(), nil, &inbox)
	if len(inbox) != 1 || inbox[0].Reason != "approval" {
		t.Fatalf("inbox should hold one approval decision, got %+v", inbox)
	}

	var errBody gen.Error
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/advance", nil, &errBody); code != 409 {
		t.Fatalf("advance without approval: %d %v", code, errBody)
	}
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/approve", nil, &task); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/advance", gen.Reason{}, &task); code != 200 || task.Phase != "implementation" {
		t.Fatalf("advance: %d %q", code, task.Phase)
	}
	readEvent("task.phase_changed")

	// failing CI blocks; inbox reason flips to blocked
	detail := "tests red"
	if code := h.do("PUT", "/tasks/"+task.Id.String()+"/checks", gen.Check{Name: "ci", Source: "plugin:github", Status: "fail", Detail: &detail}, &task); code != 200 || task.BlockedReason == nil {
		t.Fatalf("failing check should block: %d %v", code, task.BlockedReason)
	}
	readEvent("gate.blocked")
	h.do("GET", "/inbox?projectId="+project.Id.String(), nil, &inbox)
	if len(inbox) != 1 || inbox[0].Reason != "blocked" {
		t.Fatalf("inbox should show blocked, got %+v", inbox)
	}

	var d gen.TaskDetail
	h.do("GET", "/tasks/"+task.Id.String(), nil, &d)
	if d.Gate.Kind != "both" || len(d.Gate.Checks) != 1 || d.Phases[len(d.Phases)-1] != "approved" {
		t.Fatalf("detail: gate=%s checks=%d phases=%v", d.Gate.Kind, len(d.Gate.Checks), d.Phases)
	}

	if code := h.do("POST", "/tasks/"+task.Id.String()+"/rollback", gen.Rollback{To: "planning", Reason: ""}, &errBody); code != 409 && code != 500 {
		t.Fatalf("rollback with empty reason: %d", code)
	}

	var trs []gen.Transition
	h.do("GET", "/tasks/"+task.Id.String()+"/transitions", nil, &trs)
	if len(trs) < 3 {
		t.Fatalf("expected audit trail, got %d", len(trs))
	}
}

func TestApproveNeedsUserIdentity(t *testing.T) {
	h := newHarness(t)
	h.user = ""
	var project gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("q%d", time.Now().UnixNano()), Name: "Q"}, &project)
	var task gen.Task
	h.do("POST", "/projects/"+project.Id.String()+"/tasks", gen.NewTask{Kind: "chore", Title: "x"}, &task)
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/approve", nil, nil); code != 403 {
		t.Fatalf("approve as system: %d", code)
	}
}
