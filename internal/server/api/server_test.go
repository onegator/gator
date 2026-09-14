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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store"
)

type harness struct {
	t     *testing.T
	srv   *httptest.Server
	pool  *pgxpool.Pool
	token string // bearer of the current caller
	relay *events.Relay
}

// login creates a user with the given workspace role and returns a bearer token.
func (h *harness) login(role string) (userID string, token string) {
	h.t.Helper()
	ctx := context.Background()
	var uid pgtype.UUID
	if err := h.pool.QueryRow(ctx, "INSERT INTO users(email,name,workspace_role) VALUES($1,'t',$2) RETURNING id",
		fmt.Sprintf("api-%d@t.local", time.Now().UnixNano()), role).Scan(&uid); err != nil {
		h.t.Fatal(err)
	}
	plain, _, err := auth.Tokens{Pool: h.pool}.Issue(ctx, auth.IssueParams{Kind: auth.KindUser, Scope: "user", Name: "test", UserID: uid})
	if err != nil {
		h.t.Fatal(err)
	}
	v, _ := uid.Value()
	s, _ := v.(string)
	return s, plain
}

func (h *harness) member(project, userID, role string) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), "INSERT INTO memberships(project_id,user_id,role) VALUES($1,$2,$3) ON CONFLICT (project_id,user_id) DO UPDATE SET role=EXCLUDED.role", project, userID, role); err != nil {
		h.t.Fatal(err)
	}
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
	cat, _ := process.DefaultCatalog()
	svc := process.NewService(pool, process.DBTemplates{Pool: pool, Defaults: cat}, process.StaticCapabilities(nil))
	hub := events.NewHub()
	relay := &events.Relay{Pool: pool, Hub: hub, Interval: 50 * time.Millisecond, Log: slog.Default()}
	rctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go relay.Run(rctx)
	s := &Server{Pool: pool, Process: svc, Hub: hub, Features: []string{"inbox"}, Log: slog.Default(),
		Tokens: auth.Tokens{Pool: pool}, Authz: auth.Authorizer{Pool: pool}}
	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)
	h := &harness{t: t, srv: srv, pool: pool, relay: relay}
	_, h.token = h.login("admin")
	return h
}

func (h *harness) do(method, path string, body any, out any) int {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+"/api/v1"+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
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
	ws, _, err := websocket.Dial(ctx, strings.Replace(h.srv.URL, "http", "ws", 1)+"/api/v1/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + h.token}}})
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

func TestRBACOverHTTP(t *testing.T) {
	h := newHarness(t)
	admin := h.token
	var project gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("r%d", time.Now().UnixNano()), Name: "R"}, &project); code != 201 {
		t.Fatalf("admin create project: %d", code)
	}
	var task gen.Task
	if code := h.do("POST", "/projects/"+project.Id.String()+"/tasks", gen.NewTask{Kind: "chore", Title: "x"}, &task); code != 201 {
		t.Fatalf("admin create task: %d", code)
	}

	// anonymous
	h.token = ""
	if code := h.do("GET", "/inbox", nil, nil); code != 401 {
		t.Fatalf("anonymous inbox: %d", code)
	}

	// stranger: no membership
	strangerID, stranger := h.login("member")
	h.token = stranger
	if code := h.do("POST", "/projects", gen.NewProject{Slug: "nope", Name: "n"}, nil); code != 403 {
		t.Fatalf("member creating project: %d", code)
	}
	if code := h.do("GET", "/tasks/"+task.Id.String(), nil, nil); code != 403 {
		t.Fatalf("stranger reading task: %d", code)
	}
	var projects []gen.Project
	h.do("GET", "/projects", nil, &projects)
	if len(projects) != 0 {
		t.Fatalf("stranger sees %d projects", len(projects))
	}
	var inbox []gen.Decision
	h.do("GET", "/inbox", nil, &inbox)
	if len(inbox) != 0 {
		t.Fatalf("stranger inbox has %d rows", len(inbox))
	}

	// viewer: can read, cannot act
	h.member(project.Id.String(), strangerID, "viewer")
	if code := h.do("GET", "/tasks/"+task.Id.String(), nil, nil); code != 200 {
		t.Fatalf("viewer reading task: %d", code)
	}
	h.do("GET", "/inbox", nil, &inbox)
	if len(inbox) != 1 {
		t.Fatalf("viewer inbox has %d rows, want 1", len(inbox))
	}
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/approve", nil, nil); code != 403 {
		t.Fatalf("viewer approving: %d", code)
	}
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/advance", nil, nil); code != 403 {
		t.Fatalf("viewer advancing: %d", code)
	}

	// member: can act
	h.member(project.Id.String(), strangerID, "member")
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/approve", nil, &task); code != 200 {
		t.Fatalf("member approving: %d", code)
	}
	if code := h.do("PUT", "/projects/"+project.Id.String()+"/members", gen.Member{UserId: task.ProjectId, Role: "viewer"}, nil); code != 403 {
		t.Fatalf("member setting members: %d", code)
	}

	// runner token acts as member; plugin token bound to another project is forbidden
	h.token = admin
	var issued gen.IssuedToken
	if code := h.do("POST", "/tokens", gen.NewToken{Kind: "runner", Name: "mac"}, &issued); code != 201 || issued.Token == "" {
		t.Fatalf("admin issuing runner token: %d", code)
	}
	h.token = issued.Token
	if code := h.do("GET", "/tasks/"+task.Id.String(), nil, nil); code != 200 {
		t.Fatalf("runner reading task: %d", code)
	}
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/approve", nil, nil); code != 403 {
		t.Fatalf("runner approving must be forbidden: %d", code)
	}
	h.token = stranger
	if code := h.do("POST", "/tokens", gen.NewToken{Kind: "runner", Name: "x"}, nil); code != 403 {
		t.Fatalf("member issuing runner token: %d", code)
	}

	// audit rows exist for mutations with the right actor kinds
	var n int
	if err := h.pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_log WHERE target LIKE '%'||$1||'%' AND actor_kind IN ('user','runner')", task.Id.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("expected audit rows for task mutations, got %d", n)
	}
}

func TestSessionCookieAuthenticates(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: h.token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var me gen.Me
	_ = json.NewDecoder(res.Body).Decode(&me)
	if res.StatusCode != 200 || me.WorkspaceRole != "admin" {
		t.Fatalf("me via cookie: %d %+v", res.StatusCode, me)
	}
	// logout revokes the session
	req, _ = http.NewRequest("POST", h.srv.URL+"/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: h.token})
	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()
	if code := h.do("GET", "/auth/me", nil, nil); code != 401 {
		t.Fatalf("after logout: %d", code)
	}
}

func TestUsageAndMetricsOverHTTP(t *testing.T) {
	h := newHarness(t)
	admin := h.token
	var project gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("m%d", time.Now().UnixNano()), Name: "M"}, &project)
	var task gen.Task
	if code := h.do("POST", "/projects/"+project.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "leak"}, &task); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	n := func(v int64) *int64 { return &v }
	str := func(v string) *string { return &v }
	cost := 0.42
	usage := gen.UsageInput{Backend: str("claude"), Model: str("claude-opus-5"), InputTokens: n(1200), OutputTokens: n(300),
		CacheReadTokens: n(8000), DurationMs: 90_000, CostUsd: &cost, IdempotencyKey: str("job-1")}

	var ack gen.UsageAck
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/usage", usage, &ack); code != 201 || !ack.Recorded {
		t.Fatalf("first usage: %d %+v", code, ack)
	}
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/usage", usage, &ack); code != 200 || ack.Recorded {
		t.Fatalf("duplicate usage must not count: %d %+v", code, ack)
	}
	bad := gen.UsageInput{InputTokens: n(-1), DurationMs: 1}
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/usage", bad, nil); code != 400 {
		t.Fatalf("negative tokens: %d", code)
	}

	var m gen.TaskMetrics
	if code := h.do("GET", "/tasks/"+task.Id.String()+"/metrics", nil, &m); code != 200 {
		t.Fatalf("metrics: %d", code)
	}
	if m.Tokens.Total != 9500 || m.Tokens.Input != 1200 || m.Jobs != 1 || m.AgentSeconds != 90 || m.CostUsd != 0.42 || !m.CostEstimated {
		t.Fatalf("task metrics: %+v", m)
	}
	if len(m.Phases) != 1 || m.Phases[0].Phase != "planning" || m.Phases[0].Owner != "runner" || m.Phases[0].Tokens.Total != 9500 || m.LeadSeconds <= 0 {
		t.Fatalf("phase metrics: %+v", m.Phases)
	}

	var pm gen.ProjectMetrics
	if code := h.do("GET", "/projects/"+project.Id.String()+"/metrics", nil, &pm); code != 200 {
		t.Fatalf("project metrics: %d", code)
	}
	if len(pm.ByKind) != 1 || pm.ByKind[0].Kind != "bug" || pm.Totals.Tokens.Total != 9500 || pm.Totals.Tasks != 1 {
		t.Fatalf("project metrics: %+v", pm)
	}

	// a viewer may read metrics but not report usage
	viewerID, viewer := h.login("member")
	h.member(project.Id.String(), viewerID, "viewer")
	h.token = viewer
	if code := h.do("GET", "/tasks/"+task.Id.String()+"/metrics", nil, nil); code != 200 {
		t.Fatalf("viewer metrics: %d", code)
	}
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/usage", gen.UsageInput{DurationMs: 1}, nil); code != 403 {
		t.Fatalf("viewer reporting usage: %d", code)
	}
	h.token = admin

	// usage rows are append-only
	if _, err := h.pool.Exec(context.Background(), "UPDATE usage_records SET input_tokens = 0 WHERE task_id = $1", task.Id.String()); err == nil {
		t.Fatal("usage_records must be append-only")
	}
}
