// Package e2e exercises gator-server and gator-runner together. It is the only package
// allowed to import both sides; the boundary test only covers server/... and runner/....
package e2e

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/server/api"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/plugins"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/quality"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store"
)

type harness struct {
	t       *testing.T
	srv     *httptest.Server
	pool    *pgxpool.Pool
	mgr     *runners.Manager
	hub     *events.Hub
	plugins *plugins.Host
	quality *quality.Service
	svc     *process.Service
	admin   string // user bearer
	runner  string // runner bearer
}

const (
	testLeaseTTL   = 30 * time.Second
	testStallAfter = 2 * time.Second
)

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
	svc := process.NewService(pool, process.DBTemplates{Pool: pool, Defaults: cat}, plugins.Capabilities{Pool: pool})
	hub := events.NewHub()
	keys, err := secrets.ParseKeyring(secrets.NewKey("e2e"), "")
	if err != nil {
		t.Fatal(err)
	}
	host := plugins.New(pool, svc, keys, hub, slog.Default(), plugins.Config{CallTimeout: 2 * time.Second,
		MinBackoff: 50 * time.Millisecond, MaxBackoff: 200 * time.Millisecond, BreakerThreshold: 3,
		SyncEvery: time.Hour, PollEvery: 100 * time.Millisecond})
	hostCtx, stopHost := context.WithCancel(context.Background())
	hostDone := make(chan struct{})
	go func() { host.Run(hostCtx); close(hostDone) }()
	t.Cleanup(func() { stopHost(); <-hostDone })
	mgr := runners.New(pool, svc, hub, slog.Default(), runners.Config{LeaseTTL: testLeaseTTL, OfflineAfter: 20 * time.Second,
		StallAfter: testStallAfter, Preparer: host})
	scorecards := quality.New(pool, svc, host, slog.Default())
	s := &api.Server{Pool: pool, Process: svc, Runners: mgr, Plugins: host, Quality: scorecards, Hub: hub, Log: slog.Default(),
		Tokens: auth.Tokens{Pool: pool}, Authz: auth.Authorizer{Pool: pool}}
	root := chi.NewRouter()
	root.Mount("/", s.Router())
	root.Mount("/hooks", host.Hooks())
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)

	_, admin, err := auth.BootstrapAdmin(ctx, pool, fmt.Sprintf("e2e-%d@t.local", time.Now().UnixNano()), "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := auth.IssueRunnerToken(ctx, pool, fmt.Sprintf("e2e-runner-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, srv: srv, pool: pool, mgr: mgr, hub: hub, svc: svc, plugins: host, quality: scorecards, admin: admin, runner: runner}
}

func (h *harness) do(method, path string, body, out any) int {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+"/api/v1"+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.admin)
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

// task creates a project and a bug task (starts in planning, a runner phase).
func (h *harness) task() gen.Task {
	h.t.Helper()
	var p gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("e%d", time.Now().UnixNano()), Name: "E2E"}, &p); code != 201 {
		h.t.Fatalf("project: %d", code)
	}
	var task gen.Task
	if code := h.do("POST", "/projects/"+p.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "e2e"}, &task); code != 201 {
		h.t.Fatalf("task: %d", code)
	}
	return task
}

func (h *harness) job(task gen.Task, backend, instruction string, maxAttempts int) gen.Job {
	h.t.Helper()
	in := gen.NewJob{Backend: &backend, Instruction: &instruction}
	if maxAttempts > 0 {
		in.MaxAttempts = &maxAttempts
	}
	var j gen.Job
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/jobs", in, &j); code != 201 {
		h.t.Fatalf("create job: %d", code)
	}
	return j
}

func (h *harness) waitJob(id string, want ...string) gen.Job {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var j gen.Job
	for time.Now().Before(deadline) {
		h.do("GET", "/jobs/"+id, nil, &j)
		for _, w := range want {
			if string(j.Status) == w {
				return j
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("job %s stuck in %s (want %v, reason %v)", id, j.Status, want, deref(j.StopReason))
	return j
}

func (h *harness) startRunner(backend string, exec client.Executor) (*client.Client, context.CancelFunc) {
	h.t.Helper()
	c := client.New(client.Config{ServerURL: h.srv.URL, Token: h.runner, Name: "e2e", Location: "other",
		Backends: []string{backend}, MaxParallel: 2, HeartbeatInterval: 100 * time.Millisecond, MinBackoff: 50 * time.Millisecond},
		exec, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !c.Connected() {
		if time.Now().After(deadline) {
			cancel()
			h.t.Fatal("runner never connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Cleanup(cancel)
	return c, cancel
}

// --- raw protocol runner for edge cases ---

type rawRunner struct {
	t   *testing.T
	c   *websocket.Conn
	seq uint64
}

func (h *harness) dialRaw(token, backend string, version, maxParallel int) (*rawRunner, proto.RawEnvelope) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := strings.Replace(h.srv.URL, "http", "ws", 1) + proto.Path
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { c.CloseNow() })
	r := &rawRunner{t: h.t, c: c}
	reg := proto.Register{Name: "raw", Location: "other", Capabilities: proto.Capabilities{Backends: []string{backend}, MaxParallel: maxParallel}}
	b, _ := json.Marshal(proto.Envelope{Type: proto.TypeRegister, Seq: 1, Version: version, Payload: reg})
	r.seq = 1
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		h.t.Fatal(err)
	}
	return r, r.recv()
}

func (r *rawRunner) send(t proto.MessageType, payload any) {
	r.t.Helper()
	r.seq++
	b, _ := proto.Encode(t, r.seq, payload)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.c.Write(ctx, websocket.MessageText, b); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rawRunner) recv() proto.RawEnvelope {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := r.c.Read(ctx)
	if err != nil {
		r.t.Fatalf("raw recv: %v", err)
	}
	env, err := proto.Decode(b)
	if err != nil {
		r.t.Fatal(err)
	}
	return env
}

func (r *rawRunner) recvType(want proto.MessageType) proto.RawEnvelope {
	r.t.Helper()
	for i := 0; i < 20; i++ {
		env := r.recv()
		if env.Type == want {
			return env
		}
	}
	r.t.Fatalf("never received %s", want)
	return proto.RawEnvelope{}
}

func (r *rawRunner) lease(slots int) []proto.Job {
	r.t.Helper()
	r.send(proto.TypeLeaseRequest, proto.LeaseRequest{Slots: slots})
	var l proto.Lease
	if err := r.recvType(proto.TypeLease).Into(&l); err != nil {
		r.t.Fatal(err)
	}
	return l.Jobs
}

// uniqueBackend names a backend no other runner in this process answers for. The clock alone
// is not enough: two calls in the same microsecond used to return the same name, and then one
// runner could take the other's job.
var backendSeq atomic.Int64

func uniqueBackend() string {
	return fmt.Sprintf("fake-%d-%d", time.Now().UnixNano(), backendSeq.Add(1))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
