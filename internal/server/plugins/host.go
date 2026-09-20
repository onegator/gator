// Package plugins runs plugin processes and connects them to the core. Every (project,
// plugin) pair gets its own process, so a project's secrets never reach another project's
// plugin. Runners never start plugins; only gator-server does.
package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/limits"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/version"
	"github.com/onegator/gator/plugin"
)

var (
	// ErrInvalid is a bad install or configuration request.
	ErrInvalid = errors.New("invalid plugin request")
	// ErrNotRunning means the plugin's process is not up (starting, restarting or disabled).
	ErrNotRunning = errors.New("plugin is not running")
	// ErrTimeout means the plugin did not answer within the call timeout.
	ErrTimeout = errors.New("plugin call timed out")
	// ErrNoKeyring means secrets were given but GATOR_SECRETS_KEY is not configured.
	ErrNoKeyring = errors.New("plugin secrets need GATOR_SECRETS_KEY")
)

// Config tunes the host. Zero values take the defaults.
type Config struct {
	CallTimeout      time.Duration // per call; default 30s
	MinBackoff       time.Duration // first restart delay; default 1s
	MaxBackoff       time.Duration // default 1m
	BreakerThreshold int           // failed calls in a row before the plugin is disabled for the project; default 5
	SyncEvery        time.Duration // how often to reconcile processes with the database; default 30s
	PollEvery        time.Duration // how often to read new domain events for hooks; default 1s
	WebhookMaxBytes  int64         // default 1 MiB
	WebhookPerSecond float64       // per project and plugin; default 10
	WebhookBurst     int           // default 20
}

func (c *Config) defaults() {
	if c.CallTimeout == 0 {
		c.CallTimeout = 30 * time.Second
	}
	if c.MinBackoff == 0 {
		c.MinBackoff = time.Second
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = time.Minute
	}
	if c.BreakerThreshold == 0 {
		c.BreakerThreshold = 5
	}
	if c.SyncEvery == 0 {
		c.SyncEvery = 30 * time.Second
	}
	if c.PollEvery == 0 {
		c.PollEvery = time.Second
	}
	if c.WebhookMaxBytes == 0 {
		c.WebhookMaxBytes = 1 << 20
	}
	if c.WebhookPerSecond == 0 {
		c.WebhookPerSecond = 10
	}
	if c.WebhookBurst == 0 {
		c.WebhookBurst = 20
	}
}

// Host supervises plugin processes.
type Host struct {
	pool *pgxpool.Pool
	proc *process.Service
	keys *secrets.Keyring // nil: plugins cannot hold secrets
	hub  *events.Hub
	log  *slog.Logger
	cfg  Config
	lim  *limits.Limiter

	mu   sync.Mutex
	inst map[[16]byte]*instance
}

// New creates a host; Run starts it.
func New(pool *pgxpool.Pool, proc *process.Service, keys *secrets.Keyring, hub *events.Hub, log *slog.Logger, cfg Config) *Host {
	cfg.defaults()
	return &Host{pool: pool, proc: proc, keys: keys, hub: hub, log: log, cfg: cfg,
		lim: limits.NewLimiter(cfg.WebhookPerSecond, cfg.WebhookBurst), inst: map[[16]byte]*instance{}}
}

// Capabilities answers process.CapabilityResolver from the database, so it works before the
// host starts and on every node.
type Capabilities struct{ Pool *pgxpool.Pool }

// Capabilities implements process.CapabilityResolver.
func (c Capabilities) Capabilities(ctx context.Context, projectID pgtype.UUID) ([]string, error) {
	return db.New(c.Pool).ProjectCapabilities(ctx, projectID)
}

// binding is one enabled (project, plugin) pair as stored.
type binding struct {
	id, projectID pgtype.UUID
	slug, name    string
	command       []string
	manifest      plugin.Manifest
	schema        plugin.ConfigSchema
	config        map[string]any
	secrets       string // encrypted JSON object
	fingerprint   string // changes whenever the process must restart
}

func newBinding(id, projectID pgtype.UUID, slug, name string, command []string, manifest, config []byte, secretsCT string) (binding, error) {
	b := binding{id: id, projectID: projectID, slug: slug, name: name, command: command, secrets: secretsCT, config: map[string]any{}}
	if len(command) == 0 {
		return b, fmt.Errorf("plugin %s has no command", name)
	}
	if err := json.Unmarshal(manifest, &b.manifest); err != nil {
		return b, fmt.Errorf("plugin %s manifest: %w", name, err)
	}
	b.schema, _ = plugin.ParseConfigSchema(b.manifest.ConfigSchema)
	_ = json.Unmarshal(config, &b.config)
	h := sha256.New()
	for _, part := range [][]byte{[]byte(name), []byte(strings.Join(command, "\x00")), manifest, config, []byte(secretsCT)} {
		h.Write(part)
		h.Write([]byte{0})
	}
	b.fingerprint = hex.EncodeToString(h.Sum(nil))
	return b, nil
}

// Run reconciles processes with the database, delivers domain events to plugins, and stops
// every process when ctx ends. Events come from the outbox through the host's own cursor;
// the hub only wakes it early.
func (h *Host) Run(ctx context.Context) {
	wake, unsub := h.hub.Subscribe("*")
	defer unsub()
	defer h.stopAll()
	h.syncLogged(ctx)
	if err := db.New(h.pool).InitEventCursor(ctx, cursorName); err != nil && ctx.Err() == nil {
		h.log.Warn("plugin event cursor", "err", err)
	}
	syncT := time.NewTicker(h.cfg.SyncEvery)
	defer syncT.Stop()
	poll := time.NewTicker(h.cfg.PollEvery)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-syncT.C:
			h.syncLogged(ctx)
		case <-wake:
			h.drain(ctx)
		case <-poll.C:
			h.drain(ctx)
		}
	}
}

// cursorName is this consumer's row in event_cursors.
const cursorName = "plugins"

// hookEvents are the domain events plugins hear about.
var hookEvents = []string{"task.created", "task.phase_changed", "task.closed", "gate.approved", "job.finished"}

// drain delivers events after the cursor, at least once: a crash between delivery and the
// cursor update repeats a batch, which hooks must tolerate.
func (h *Host) drain(ctx context.Context) {
	q := db.New(h.pool)
	for ctx.Err() == nil {
		cur, err := q.GetEventCursor(ctx, cursorName)
		if err != nil {
			return
		}
		rows, err := q.EventsAfter(ctx, db.EventsAfterParams{After: cur, Types: hookEvents, MaxRows: 100})
		if err != nil || len(rows) == 0 {
			return
		}
		for _, r := range rows {
			if h.active() {
				h.onEvent(ctx, events.Event{ID: r.ID, Type: r.Type, Aggregate: r.Aggregate, AggregateID: uuidString(r.AggregateID), Payload: r.Payload})
			}
		}
		if err := q.SetEventCursor(ctx, db.SetEventCursorParams{Name: cursorName, EventID: rows[len(rows)-1].ID}); err != nil {
			return
		}
	}
}

func (h *Host) active() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.inst) > 0
}

func (h *Host) syncLogged(ctx context.Context) {
	if err := h.Sync(ctx); err != nil && ctx.Err() == nil {
		h.log.Warn("plugin sync", "err", err)
	}
}

// Sync starts processes for enabled bindings and stops or restarts the rest.
func (h *Host) Sync(ctx context.Context) error {
	rows, err := db.New(h.pool).ListActiveProjectPlugins(ctx)
	if err != nil {
		return err
	}
	want := map[[16]byte]binding{}
	for _, r := range rows {
		b, err := newBinding(r.ID, r.ProjectID, r.ProjectSlug, r.PluginName, r.Command, r.Manifest, r.Config, r.Secrets)
		if err != nil {
			h.log.Warn("plugin binding", "err", err)
			continue
		}
		want[r.ID.Bytes] = b
	}
	h.mu.Lock()
	var stale []*instance
	for id, i := range h.inst {
		if w, ok := want[id]; !ok || w.fingerprint != i.b.fingerprint {
			stale = append(stale, i)
			delete(h.inst, id)
		}
	}
	h.mu.Unlock()
	for _, i := range stale {
		i.stop()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, b := range want {
		if _, ok := h.inst[id]; !ok {
			h.inst[id] = h.startInstance(b)
		}
	}
	return nil
}

func (h *Host) stopAll() {
	h.mu.Lock()
	all := make([]*instance, 0, len(h.inst))
	for id, i := range h.inst {
		all = append(all, i)
		delete(h.inst, id)
	}
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, i := range all {
		wg.Add(1)
		go func(i *instance) { defer wg.Done(); i.stop() }(i)
	}
	wg.Wait()
}

func (h *Host) get(id pgtype.UUID) *instance {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inst[id.Bytes]
}

// forProject returns the running bindings of a project that handle hook ("" = any).
func (h *Host) forProject(projectID pgtype.UUID, hook string) []*instance {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*instance
	for _, i := range h.inst {
		if i.b.projectID == projectID && (hook == "" || i.b.manifest.HasHook(hook)) {
			out = append(out, i)
		}
	}
	return out
}

// instance is one supervised plugin process and its restarts.
type instance struct {
	h      *Host
	b      binding
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	conn     *plugin.Conn
	ready    chan struct{} // closed while conn is set
	failures int
	// breaking stops a burst of concurrent failures from disabling the plugin repeatedly.
	breaking   bool
	secretVals []string
}

func (h *Host) startInstance(b binding) *instance {
	ctx, cancel := context.WithCancel(context.Background())
	i := &instance{h: h, b: b, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{})}
	go i.run(ctx)
	go i.schedules(ctx)
	return i
}

func (i *instance) stop() {
	i.cancel()
	<-i.done
}

func (i *instance) log() *slog.Logger {
	return i.h.log.With("plugin", i.b.name, "project", i.b.slug)
}

func (i *instance) run(ctx context.Context) {
	defer close(i.done)
	backoff := i.h.cfg.MinBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := i.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = i.h.cfg.MinBackoff
		}
		i.log().Warn("plugin stopped; restarting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, i.h.cfg.MaxBackoff)
	}
}

// env is the plugin's whole environment: never the server's own (database URL, keyring),
// and only this project's secrets.
func (i *instance) env() ([]string, []string, error) {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir(),
		"LANG=C.UTF-8", "GATOR_PLUGIN_PROJECT=" + i.b.slug}
	if i.b.secrets == "" {
		return env, nil, nil
	}
	sec, err := i.h.decryptSecrets(i.b.secrets)
	if err != nil {
		return nil, nil, err
	}
	var vals []string
	for k, v := range sec {
		env = append(env, plugin.SecretEnv(k)+"="+v)
		vals = append(vals, v)
	}
	return env, vals, nil
}

func (h *Host) decryptSecrets(ct string) (map[string]string, error) {
	out := map[string]string{}
	if ct == "" {
		return out, nil
	}
	if h.keys == nil {
		return nil, ErrNoKeyring
	}
	b, err := h.keys.Decrypt(ct)
	if err != nil {
		return nil, err
	}
	return out, json.Unmarshal(b, &out)
}

// spawn starts the command and connects to it.
func spawn(command, env []string, stderr *logWriter, handler plugin.Handler) (*exec.Cmd, *plugin.Conn, <-chan error, func() error, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, nil, err
	}
	conn := plugin.NewConn(stdout, stdin, handler)
	waited := make(chan error, 1)
	go func() {
		<-conn.Done()
		waited <- cmd.Wait()
	}()
	return cmd, conn, waited, stdin.Close, nil
}

func kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (i *instance) session(ctx context.Context) error {
	env, vals, err := i.env()
	if err != nil {
		return err
	}
	i.mu.Lock()
	i.secretVals = vals
	i.mu.Unlock()
	cmd, conn, waited, closeStdin, err := spawn(i.b.command, env, &logWriter{log: i.log()}, i.handleCore)
	if err != nil {
		return err
	}
	initCtx, cancel := context.WithTimeout(ctx, i.h.cfg.CallTimeout)
	var m plugin.Manifest
	err = conn.Call(initCtx, plugin.MethodInitialize, plugin.InitializeParams{
		CoreVersion: version.Version, ProtocolVersion: plugin.ProtocolVersion,
		ProjectID: uuidString(i.b.projectID), ProjectSlug: i.b.slug, Config: i.b.config,
	}, &m)
	cancel()
	if err == nil && m.Name != i.b.name {
		err = fmt.Errorf("process reports name %q", m.Name)
	}
	if err != nil {
		kill(cmd)
		<-waited
		return fmt.Errorf("initialize: %w", err)
	}
	i.setConn(conn)
	defer i.setConn(nil)
	i.log().Info("plugin started", "pid", cmd.Process.Pid, "version", m.Version)
	select {
	case err := <-waited:
		return fmt.Errorf("process exited: %v", err)
	case <-ctx.Done():
		_ = conn.Notify(plugin.MethodShutdown, nil)
		_ = closeStdin()
		select {
		case <-waited:
		case <-time.After(3 * time.Second):
			kill(cmd)
			<-waited
		}
		return ctx.Err()
	}
}

func (i *instance) setConn(c *plugin.Conn) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if c != nil {
		i.conn = c
		close(i.ready)
		return
	}
	i.conn = nil
	i.ready = make(chan struct{})
}

func (i *instance) running() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.conn != nil
}

func (i *instance) waitConn(ctx context.Context) (*plugin.Conn, error) {
	for {
		i.mu.Lock()
		c, r := i.conn, i.ready
		i.mu.Unlock()
		if c != nil {
			return c, nil
		}
		select {
		case <-r:
		case <-ctx.Done():
			return nil, ErrNotRunning
		case <-i.done:
			return nil, ErrNotRunning
		}
	}
}

func (i *instance) schedules(ctx context.Context) {
	if !i.b.manifest.HasHook(plugin.MethodSchedule) {
		return
	}
	for _, s := range i.b.manifest.Schedule {
		d, err := time.ParseDuration(s.Every)
		if err != nil || d < time.Second {
			continue
		}
		go func(name string, d time.Duration) {
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := i.h.call(ctx, i, plugin.MethodSchedule, plugin.ScheduleParams{Name: name}, nil); err != nil && ctx.Err() == nil {
						i.log().Warn("scheduled call", "schedule", name, "err", err)
					}
				}
			}
		}(s.Name, d)
	}
}

// call invokes a hook with the timeout, audit and failure accounting.
func (h *Host) call(ctx context.Context, i *instance, method string, params, out any) error {
	cctx, cancel := context.WithTimeout(ctx, h.cfg.CallTimeout)
	defer cancel()
	start := time.Now()
	var raw json.RawMessage
	conn, err := i.waitConn(cctx)
	if err == nil {
		err = conn.Call(cctx, method, params, &raw)
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w: %s after %s", ErrTimeout, method, h.cfg.CallTimeout)
		}
	}
	i.audit("core_to_plugin", method, params, raw, err, time.Since(start))
	i.outcome(err)
	if err != nil {
		return err
	}
	if out != nil && len(raw) > 0 && string(raw) != "null" {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// outcome counts transport failures (no answer, crash, panic); a plugin's own refusal, such
// as a bad webhook signature, does not count, so outsiders cannot switch a plugin off.
func (i *instance) outcome(err error) {
	var re *plugin.Error
	counts := err != nil && (errors.Is(err, ErrTimeout) || errors.Is(err, ErrNotRunning) || errors.Is(err, plugin.ErrClosed) ||
		(errors.As(err, &re) && re.Code == plugin.CodeInternal))
	i.mu.Lock()
	switch {
	case err == nil:
		i.failures = 0
		i.breaking = false
	case counts:
		i.failures++
	}
	n := i.failures
	// Hooks run concurrently, so two failures can land at once and step over an exact
	// threshold. Fire at or past it, and only once per run of failures.
	trip := counts && n >= i.h.cfg.BreakerThreshold && !i.breaking
	if trip {
		i.breaking = true
	}
	i.mu.Unlock()
	if trip {
		go i.h.disable(i, fmt.Sprintf("disabled after %d failed calls in a row; last: %v", n, err))
	}
}

func (h *Host) disable(i *instance, reason string) {
	ctx := context.Background()
	q := db.New(h.pool)
	if err := q.DisableProjectPlugin(ctx, db.DisableProjectPluginParams{ID: i.b.id, DisabledReason: &reason}); err != nil {
		h.log.Error("disable plugin", "plugin", i.b.name, "err", err)
		return
	}
	_ = emit(ctx, q, "plugin.disabled", "project", i.b.projectID, map[string]any{"plugin": i.b.name, "reason": reason})
	i.log().Error("plugin disabled for project", "reason", reason)
	h.mu.Lock()
	if h.inst[i.b.id.Bytes] == i {
		delete(h.inst, i.b.id.Bytes)
	}
	h.mu.Unlock()
	i.stop()
}

const maxAuditBytes = 64 << 10

// audit records one call with secrets removed, by field name and by value.
func (i *instance) audit(direction, method string, params any, result json.RawMessage, callErr error, d time.Duration) {
	var p []byte
	switch v := params.(type) {
	case nil:
	case json.RawMessage:
		p = v
	default:
		p, _ = json.Marshal(v)
	}
	var e *string
	if callErr != nil {
		s := string(i.scrub([]byte(callErr.Error())))
		e = &s
	}
	err := db.New(i.h.pool).InsertPluginCall(context.Background(), db.InsertPluginCallParams{
		ProjectPluginID: i.b.id, Direction: direction, Method: method,
		Payload: i.redact(p), Result: i.redact(result), Error: e, DurationMs: int32(d.Milliseconds()),
	})
	if err != nil {
		i.log().Warn("plugin audit", "err", err)
	}
}

func (i *instance) redact(b []byte) []byte {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if len(b) > maxAuditBytes {
		return []byte(fmt.Sprintf(`{"truncated":true,"bytes":%d}`, len(b)))
	}
	b = secrets.RedactJSON(b, i.b.schema.SecretKeys()...)
	b = i.scrub(b)
	if !json.Valid(b) {
		b, _ = json.Marshal(string(b))
	}
	return b
}

func (i *instance) scrub(b []byte) []byte {
	i.mu.Lock()
	vals := i.secretVals
	i.mu.Unlock()
	for _, v := range vals {
		if len(v) >= 4 {
			b = bytes.ReplaceAll(b, []byte(v), []byte("[redacted]"))
		}
	}
	return b
}

// logWriter turns a plugin's stderr into log lines.
type logWriter struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		n := bytes.IndexByte(w.buf, '\n')
		if n < 0 {
			break
		}
		w.log.Info("plugin stderr", "line", string(w.buf[:n]))
		w.buf = w.buf[n+1:]
	}
	if len(w.buf) > 64<<10 {
		w.log.Info("plugin stderr", "line", string(w.buf))
		w.buf = nil
	}
	return len(p), nil
}

func emit(ctx context.Context, q *db.Queries, typ, aggregate string, id pgtype.UUID, payload map[string]any) error {
	b, _ := json.Marshal(payload)
	_, err := q.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: aggregate, AggregateID: id, Payload: b})
	return err
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func parseUUID(s string) (pgtype.UUID, bool) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil || !u.Valid {
		return u, false
	}
	return u, true
}

func envPath() string { return os.Getenv("PATH") }
