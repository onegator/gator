// Package client is the runner side of the runner protocol: it keeps one WebSocket to
// gator-server alive, registers, heartbeats, leases jobs, runs them through an Executor
// and delivers events and receipts reliably (kept until acked, resent after reconnect).
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/onegator/gator/internal/proto"
)

// JobIO is what an executor gets besides the job.
type JobIO struct {
	// Emit streams one event (text, tool call, commit…). Delivery is reliable.
	Emit func(typ string, payload any)
	// Steer receives corrections sent by a person while the job runs.
	Steer <-chan string
}

// Executor runs one job and returns its receipt. It must stop when ctx is cancelled
// (a stop request or the job's timeout) and report the matching status.
type Executor interface {
	Run(ctx context.Context, job proto.Job, io JobIO) proto.Finish
}

// Loginer is implemented by executors that can drive an interactive backend login.
type Loginer interface {
	Login(ctx context.Context, backend string) (proto.LoginPrompt, error)
}

// AuthReporter is implemented by executors that know which backends are logged in.
type AuthReporter interface {
	AuthState() map[string]string
}

// Config for a runner.
type Config struct {
	ServerURL     string // https://gator.example.ts.net (the protocol path is appended) or a full ws(s):// URL
	Token         string
	Name          string
	Location      string
	BinaryVersion string
	Backends      []string
	Projects      []string
	MaxParallel   int

	// JournalPath keeps unacked events and finishes on disk so a restarted runner still
	// delivers them. Empty keeps them in memory only.
	JournalPath string

	HeartbeatInterval time.Duration
	MinBackoff        time.Duration
	MaxBackoff        time.Duration
}

// WebSocketURL turns the configured server URL into the runner endpoint.
func WebSocketURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("server URL must be http(s) or ws(s), got %q", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = proto.Path
	}
	return u.String(), nil
}

type outMsg struct {
	typ     proto.MessageType
	payload any
	jobID   string // set for finish messages
}

type runningJob struct {
	cancel context.CancelCauseFunc
	steer  chan string
}

// Client is one runner.
type Client struct {
	cfg  Config
	exec Executor
	log  *slog.Logger

	mu       sync.Mutex
	conn     *websocket.Conn
	connCtx  context.Context
	seq      uint64
	pending  []*outMsg
	inflight map[uint64]*outMsg
	jobs     map[string]*runningJob
	journal  *fileJournal
	loaded   sync.Once
}

// New builds a client.
func New(cfg Config, exec Executor, log *slog.Logger) *Client {
	if cfg.MaxParallel < 1 {
		cfg.MaxParallel = 1
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = proto.HeartbeatInterval
	}
	if cfg.MinBackoff == 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	c := &Client{cfg: cfg, exec: exec, log: log, jobs: map[string]*runningJob{}, inflight: map[uint64]*outMsg{}}
	if cfg.JournalPath != "" {
		c.journal = &fileJournal{path: cfg.JournalPath}
	}
	return c
}

// restore loads unacked messages left by a previous process. A finish keeps its job in the
// heartbeat so the lease survives until the server acks the receipt.
func (c *Client) restore() {
	if c.journal == nil {
		return
	}
	entries, err := c.journal.load()
	if err != nil {
		c.log.Warn("journal unreadable; starting empty", "err", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range entries {
		c.pending = append(c.pending, &outMsg{typ: e.Type, payload: e.Payload, jobID: e.JobID})
		if e.Type == proto.TypeFinish && e.JobID != "" {
			c.jobs[e.JobID] = &runningJob{}
		}
	}
	if len(entries) > 0 {
		c.log.Info("restored unacked messages", "count", len(entries))
	}
}

// persistLocked writes the pending list to the journal. Caller holds c.mu.
func (c *Client) persistLocked() {
	if c.journal == nil {
		return
	}
	entries := make([]journalEntry, 0, len(c.pending))
	for _, m := range c.pending {
		b, err := json.Marshal(m.payload)
		if err != nil {
			continue
		}
		entries = append(entries, journalEntry{Type: m.typ, Payload: b, JobID: m.jobID})
	}
	if err := c.journal.save(entries); err != nil {
		c.log.Warn("journal write failed", "err", err)
	}
}

// ErrFatal is returned when the server refuses the runner for good (bad token, protocol).
var ErrFatal = errors.New("server refused the runner")

// Run keeps a connection alive until ctx is done. It returns ErrFatal-wrapped errors when
// retrying cannot help.
func (c *Client) Run(ctx context.Context) error {
	c.loaded.Do(c.restore)
	backoff := c.cfg.MinBackoff
	for {
		registered, err := c.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrFatal) {
			return err
		}
		if registered {
			backoff = c.cfg.MinBackoff
		}
		c.log.Warn("runner connection lost; reconnecting", "err", err, "in", backoff)
		sleep := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(sleep):
		}
		backoff *= 2
		if backoff > c.cfg.MaxBackoff {
			backoff = c.cfg.MaxBackoff
		}
	}
}

// Connected reports whether a registered connection is live.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// ActiveJobs returns the ids of jobs this runner holds (running or awaiting a finish ack).
func (c *Client) ActiveJobs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeLocked()
}

func (c *Client) activeLocked() []string {
	out := make([]string, 0, len(c.jobs))
	for id := range c.jobs {
		out = append(out, id)
	}
	return out
}

func (c *Client) session(ctx context.Context) (registered bool, err error) {
	wsURL, err := WebSocketURL(c.cfg.ServerURL)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrFatal, err)
	}
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	conn, resp, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + c.cfg.Token}}})
	dcancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return false, fmt.Errorf("%w: %s", ErrFatal, resp.Status)
		}
		return false, err
	}
	conn.SetReadLimit(4 << 20)
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.CloseNow()

	reg := proto.Register{Name: c.cfg.Name, Location: c.cfg.Location, BinaryVersion: c.cfg.BinaryVersion,
		Capabilities: proto.Capabilities{Backends: c.cfg.Backends, MaxParallel: c.cfg.MaxParallel, Projects: c.cfg.Projects}}
	if err := write(sctx, conn, proto.TypeRegister, 1, reg); err != nil {
		return false, err
	}
	rctx, rcancel := context.WithTimeout(sctx, 15*time.Second)
	_, b, err := conn.Read(rctx)
	rcancel()
	if err != nil {
		return false, err
	}
	env, err := proto.Decode(b)
	if err != nil {
		return false, err
	}
	if env.Type == proto.TypeError {
		var e proto.Error
		_ = env.Into(&e)
		return false, fmt.Errorf("%w: %s: %s", ErrFatal, e.Code, e.Message)
	}
	if env.Type != proto.TypeRegistered {
		return false, fmt.Errorf("expected registered, got %s", env.Type)
	}
	var ok proto.Registered
	_ = env.Into(&ok)
	c.log.Info("runner registered", "runner_id", ok.RunnerID, "proto", ok.Version)

	c.attach(sctx, conn)
	defer c.detach(conn)

	go c.heartbeats(sctx)
	return true, c.readLoop(sctx, conn)
}

// attach makes conn current and resends every unacked reliable message on it.
func (c *Client) attach(ctx context.Context, conn *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn, c.connCtx, c.seq = conn, ctx, 1
	c.inflight = map[uint64]*outMsg{}
	for _, m := range c.pending {
		c.writeLocked(m)
	}
}

func (c *Client) detach(conn *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == conn {
		c.conn, c.connCtx = nil, nil
	}
}

// writeLocked sends m on the current connection; reliable messages are tracked by seq.
func (c *Client) writeLocked(m *outMsg) {
	if c.conn == nil {
		return
	}
	c.seq++
	if m.typ == proto.TypeEvents || m.typ == proto.TypeFinish {
		c.inflight[c.seq] = m
	}
	if err := write(c.connCtx, c.conn, m.typ, c.seq, m.payload); err != nil {
		c.log.Debug("write failed; will resend after reconnect", "type", m.typ, "err", err)
	}
}

func (c *Client) sendReliable(m *outMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, m)
	c.persistLocked()
	c.writeLocked(m)
}

func (c *Client) sendBestEffort(t proto.MessageType, payload any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeLocked(&outMsg{typ: t, payload: payload})
}

func (c *Client) ack(seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.inflight[seq]
	if !ok {
		return
	}
	delete(c.inflight, seq)
	for i, p := range c.pending {
		if p == m {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			break
		}
	}
	c.persistLocked()
	if m.typ == proto.TypeFinish {
		delete(c.jobs, m.jobID) // only now may the heartbeat stop listing it
	}
}

func (c *Client) heartbeats(ctx context.Context) {
	t := time.NewTicker(c.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		c.beat()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Client) beat() {
	authState := map[string]string{}
	if ar, ok := c.exec.(AuthReporter); ok {
		authState = ar.AuthState()
	}
	c.mu.Lock()
	active := c.activeLocked()
	free := c.cfg.MaxParallel - len(c.jobs)
	c.mu.Unlock()
	c.sendBestEffort(proto.TypeHeartbeat, proto.Heartbeat{Load: len(active), AuthState: authState, ActiveJobs: active})
	if free > 0 && len(c.cfg.Backends) > 0 {
		c.sendBestEffort(proto.TypeLeaseRequest, proto.LeaseRequest{Slots: free})
	}
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		env, err := proto.Decode(b)
		if err != nil {
			c.log.Warn("bad message from server", "err", err)
			continue
		}
		switch env.Type {
		case proto.TypeAck:
			var a proto.Ack
			if env.Into(&a) == nil {
				c.ack(a.Seq)
			}
		case proto.TypeLease:
			var l proto.Lease
			if env.Into(&l) == nil {
				for _, j := range l.Jobs {
					c.start(ctx, j)
				}
			}
		case proto.TypeSteer:
			var st proto.Steer
			if env.Into(&st) == nil {
				c.mu.Lock()
				if rj := c.jobs[st.JobID]; rj != nil {
					select {
					case rj.steer <- st.Message:
					default:
						c.log.Warn("steer dropped; job not reading", "job", st.JobID)
					}
				}
				c.mu.Unlock()
			}
		case proto.TypeStop:
			var sp proto.Stop
			if env.Into(&sp) == nil {
				c.mu.Lock()
				if rj := c.jobs[sp.JobID]; rj != nil && rj.cancel != nil {
					rj.cancel(fmt.Errorf("stopped: %s", sp.Reason))
				}
				c.mu.Unlock()
			}
		case proto.TypeLoginBackend:
			var lb proto.LoginBackend
			if env.Into(&lb) == nil {
				go c.login(ctx, lb.Backend)
			}
		case proto.TypeError:
			var e proto.Error
			_ = env.Into(&e)
			c.log.Warn("server error", "code", e.Code, "message", e.Message)
			if e.Fatal {
				return fmt.Errorf("%w: %s", ErrFatal, e.Message)
			}
		}
	}
}

func (c *Client) login(ctx context.Context, backend string) {
	l, ok := c.exec.(Loginer)
	if !ok {
		c.log.Warn("login requested but this runner cannot drive logins", "backend", backend)
		return
	}
	p, err := l.Login(ctx, backend)
	if err != nil {
		c.log.Warn("backend login failed", "backend", backend, "err", err)
		return
	}
	c.sendBestEffort(proto.TypeLoginPrompt, p)
}

// start runs a leased job unless this runner already holds it (a duplicate lease).
func (c *Client) start(parent context.Context, job proto.Job) {
	c.mu.Lock()
	if _, dup := c.jobs[job.JobID]; dup {
		c.mu.Unlock()
		return
	}
	// Jobs outlive a single connection: base them on a background context, not the session.
	jctx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	if job.Bounds.TimeoutSeconds > 0 {
		var tcancel context.CancelFunc
		jctx, tcancel = context.WithTimeoutCause(jctx, time.Duration(job.Bounds.TimeoutSeconds)*time.Second, errors.New("job timeout"))
		prev := cancel
		cancel = func(err error) { prev(err); tcancel() }
	}
	rj := &runningJob{cancel: cancel, steer: make(chan string, 8)}
	c.jobs[job.JobID] = rj
	c.mu.Unlock()

	go func() {
		var seq uint64
		var seqMu sync.Mutex
		emit := func(typ string, payload any) {
			b, err := json.Marshal(payload)
			if err != nil {
				b, _ = json.Marshal(map[string]string{"error": err.Error()})
			}
			seqMu.Lock()
			seq++
			ev := proto.JobEvent{Seq: seq, Type: typ, At: time.Now(), Payload: b}
			seqMu.Unlock()
			c.sendReliable(&outMsg{typ: proto.TypeEvents, payload: proto.Events{JobID: job.JobID, Events: []proto.JobEvent{ev}}})
		}
		fin := c.exec.Run(jctx, job, JobIO{Emit: emit, Steer: rj.steer})
		cause := context.Cause(jctx)
		cancel(nil)
		fin.JobID = job.JobID
		if fin.Status == "" {
			fin.Status = proto.StatusFailed
		}
		if cause != nil && fin.Status == proto.StatusDone {
			// The executor ignored cancellation; the receipt must say what really happened.
			fin.Status, fin.StopReason = proto.StatusFailed, "finished after cancellation: "+cause.Error()
		}
		if cause != nil && fin.StopReason == "" {
			fin.StopReason = cause.Error()
		}
		c.sendReliable(&outMsg{typ: proto.TypeFinish, payload: fin, jobID: job.JobID})
		c.beat() // free slot: ask for more work right away
	}()
}

func write(ctx context.Context, conn *websocket.Conn, t proto.MessageType, seq uint64, payload any) error {
	b, err := proto.Encode(t, seq, payload)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

// Unconfigured is the executor used until agent backends land (PLQ-223). A runner with
// no backends never receives jobs, so Run is only reached by misconfiguration.
type Unconfigured struct{}

// Run fails the job with an explicit reason.
func (Unconfigured) Run(context.Context, proto.Job, JobIO) proto.Finish {
	return proto.Finish{Status: proto.StatusFailed, StopReason: "this runner has no agent backend configured"}
}

// SplitList parses a comma-separated list, dropping empties.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
