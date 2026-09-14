// Package runners owns the server side of the runner protocol: connected sessions,
// job creation and dispatch, leases, stall and offline detection, and job receipts.
package runners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/server/telemetry"
)

var (
	ErrInvalidJob    = errors.New("invalid job")
	ErrJobFinished   = errors.New("job already finished")
	ErrRunnerOffline = errors.New("runner is not connected")
	ErrJobNotActive  = errors.New("job is not running")
)

// Config tunes timing. Zero values take the protocol defaults.
type Config struct {
	OfflineAfter    time.Duration
	LeaseTTL        time.Duration
	StallAfter      time.Duration // running job with no events for this long becomes stalled
	SweepEvery      time.Duration
	RegisterTimeout time.Duration
}

func (c *Config) defaults() {
	if c.OfflineAfter == 0 {
		c.OfflineAfter = proto.OfflineAfter
	}
	if c.LeaseTTL == 0 {
		c.LeaseTTL = proto.LeaseTTL
	}
	if c.StallAfter == 0 {
		c.StallAfter = 10 * time.Minute
	}
	if c.SweepEvery == 0 {
		c.SweepEvery = 10 * time.Second
	}
	if c.RegisterTimeout == 0 {
		c.RegisterTimeout = 10 * time.Second
	}
}

// Default job bounds, borrowed from limen: one job is one short turn.
const (
	DefaultTimeoutSeconds = 90 * 60
	DefaultMaxToolCalls   = 900
	DefaultMaxAttempts    = 3
)

// Manager coordinates every runner connection.
type Manager struct {
	pool    *pgxpool.Pool
	process *process.Service
	hub     *events.Hub
	log     *slog.Logger
	cfg     Config
	now     func() time.Time

	mu       sync.Mutex
	sessions map[[16]byte]*session
}

// New builds a manager.
func New(pool *pgxpool.Pool, proc *process.Service, hub *events.Hub, log *slog.Logger, cfg Config) *Manager {
	cfg.defaults()
	return &Manager{pool: pool, process: proc, hub: hub, log: log, cfg: cfg, now: time.Now, sessions: map[[16]byte]*session{}}
}

// Connected reports whether a runner has a live session.
func (m *Manager) Connected(runnerID pgtype.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[runnerID.Bytes]
	return ok
}

func (m *Manager) session(runnerID pgtype.UUID) *session {
	if !runnerID.Valid {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[runnerID.Bytes]
}

// ServeHTTP accepts a runner WebSocket. The caller must be authenticated with a runner token.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.Kind != auth.KindRunner {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"runner token required","code":"forbidden"}`))
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(4 << 20)
	m.serve(r.Context(), c, p)
}

// --- jobs ---

// NewJob describes a job to create for a task's current phase.
type NewJob struct {
	Backend      string
	Instruction  string
	Role         string // defaults to the phase's runner role
	MaxAttempts  int
	Bounds       proto.Bounds
	CreatedBy    process.Actor
	TaskOverride pgtype.UUID // unused; reserved
}

// CreateJob queues a job for the task's current phase and offers it to connected runners.
func (m *Manager) CreateJob(ctx context.Context, taskID pgtype.UUID, in NewJob) (db.Job, error) {
	if strings.TrimSpace(in.Backend) == "" {
		return db.Job{}, fmt.Errorf("%w: backend is required", ErrInvalidJob)
	}
	d, err := m.process.Detail(ctx, taskID)
	if err != nil {
		return db.Job{}, err
	}
	if d.Task.ClosedAt.Valid {
		return db.Job{}, fmt.Errorf("%w: task is closed", ErrInvalidJob)
	}
	role := in.Role
	if role == "" {
		role = d.Phase.Role
	}
	if role == "" {
		return db.Job{}, fmt.Errorf("%w: phase %q has no runner role; pass one explicitly", ErrInvalidJob, d.Task.Phase)
	}
	if in.MaxAttempts <= 0 {
		in.MaxAttempts = DefaultMaxAttempts
	}
	if in.Bounds.TimeoutSeconds <= 0 {
		in.Bounds.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if in.Bounds.MaxToolCalls <= 0 {
		in.Bounds.MaxToolCalls = DefaultMaxToolCalls
	}
	bounds, _ := json.Marshal(in.Bounds)
	var job db.Job
	err = m.tx(ctx, func(q *db.Queries) error {
		var err error
		job, err = q.CreateJob(ctx, db.CreateJobParams{
			TaskID: taskID, ProjectID: d.Task.ProjectID, Phase: d.Task.Phase, Role: role,
			Backend: in.Backend, Instruction: in.Instruction, Bounds: bounds, MaxAttempts: int32(in.MaxAttempts),
			CreatedByKind: string(in.CreatedBy.Kind), CreatedBy: in.CreatedBy.ID,
		})
		if err != nil {
			return err
		}
		return m.emitJob(ctx, q, "job.queued", job, nil)
	})
	if err != nil {
		return db.Job{}, err
	}
	m.Offer()
	return job, nil
}

// StopJob stops a queued job immediately, asks a connected runner to stop an active one,
// and finishes an active job server-side when its runner is gone.
func (m *Manager) StopJob(ctx context.Context, jobID pgtype.UUID, reason string) error {
	if reason == "" {
		reason = "stopped by request"
	}
	q := db.New(m.pool)
	n, err := q.StopQueuedJob(ctx, db.StopQueuedJobParams{ID: jobID, StopReason: &reason})
	if err != nil {
		return err
	}
	job, err := q.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if n == 1 {
		return m.tx(ctx, func(q *db.Queries) error { return m.emitJob(ctx, q, "job.finished", job, nil) })
	}
	if terminal(job.Status) {
		return ErrJobFinished
	}
	if s := m.session(job.RunnerID); s != nil {
		return s.send(proto.TypeStop, proto.Stop{JobID: uuidString(jobID), Reason: reason})
	}
	msg := "stopped while its runner was offline: " + reason
	return m.tx(ctx, func(q *db.Queries) error {
		j, err := q.GetJobForUpdate(ctx, jobID)
		if err != nil {
			return err
		}
		if terminal(j.Status) {
			return nil
		}
		if err := q.FinishJob(ctx, db.FinishJobParams{ID: jobID, Status: proto.StatusStopped, StopReason: &msg}); err != nil {
			return err
		}
		j.Status = proto.StatusStopped
		return m.emitJob(ctx, q, "job.finished", j, nil)
	})
}

// SteerJob forwards a correction to the runner working on the job.
func (m *Manager) SteerJob(ctx context.Context, jobID pgtype.UUID, message string) error {
	job, err := db.New(m.pool).GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Status != "leased" && job.Status != "running" && job.Status != "stalled" {
		return ErrJobNotActive
	}
	s := m.session(job.RunnerID)
	if s == nil {
		return ErrRunnerOffline
	}
	return s.send(proto.TypeSteer, proto.Steer{JobID: uuidString(jobID), Message: message})
}

// RequestLogin asks a runner to start an interactive backend login.
func (m *Manager) RequestLogin(runnerID pgtype.UUID, backend string) error {
	s := m.session(runnerID)
	if s == nil {
		return ErrRunnerOffline
	}
	return s.send(proto.TypeLoginBackend, proto.LoginBackend{Backend: backend})
}

// Offer tries to hand queued work to every connected runner with free slots.
func (m *Manager) Offer() {
	m.mu.Lock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		go func(s *session) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.dispatch(ctx, 0, false); err != nil {
				m.log.Warn("offer failed", "runner", s.name, "err", err)
			}
		}(s)
	}
}

// --- sweeping ---

// SweepResult counts what one sweep changed.
type SweepResult struct {
	Offline, Requeued, Failed, Stalled int
}

// Sweep marks silent runners offline, returns expired leases to the queue (or fails them
// after max attempts) and marks silent running jobs stalled. Safe to run concurrently.
func (m *Manager) Sweep(ctx context.Context, now time.Time) (SweepResult, error) {
	var res SweepResult
	q := db.New(m.pool)
	silent, err := q.MarkSilentRunnersOffline(ctx, ts(now.Add(-m.cfg.OfflineAfter)))
	if err != nil {
		return res, err
	}
	for _, r := range silent {
		res.Offline++
		if s := m.session(r.ID); s != nil {
			s.close("no heartbeat")
		}
		_ = m.tx(ctx, func(q *db.Queries) error {
			return emit(ctx, q, "runner.offline", "runner", r.ID, map[string]any{"name": r.Name, "reason": "no heartbeat"})
		})
	}
	expired, err := q.RequeueExpiredLeases(ctx, ts(now))
	if err != nil {
		return res, err
	}
	for _, j := range expired {
		typ := "job.requeued"
		if j.Status == proto.StatusFailed {
			typ = "job.finished"
			res.Failed++
		} else {
			res.Requeued++
		}
		_ = m.tx(ctx, func(q *db.Queries) error {
			if err := emit(ctx, q, typ, "job", j.ID, map[string]any{"task_id": uuidString(j.TaskID), "status": j.Status, "reason": "lease expired"}); err != nil {
				return err
			}
			return emit(ctx, q, "task.job_"+strings.TrimPrefix(typ, "job."), "task", j.TaskID, map[string]any{"job_id": uuidString(j.ID), "status": j.Status})
		})
	}
	stalled, err := q.MarkStalledJobs(ctx, ts(now.Add(-m.cfg.StallAfter)))
	if err != nil {
		return res, err
	}
	for _, j := range stalled {
		res.Stalled++
		_ = m.tx(ctx, func(q *db.Queries) error {
			if err := emit(ctx, q, "job.stalled", "job", j.ID, map[string]any{"task_id": uuidString(j.TaskID)}); err != nil {
				return err
			}
			return emit(ctx, q, "task.job_stalled", "task", j.TaskID, map[string]any{"job_id": uuidString(j.ID)})
		})
	}
	if res.Requeued > 0 {
		m.Offer()
	}
	return res, nil
}

// Run sweeps until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.cfg.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			res, err := m.Sweep(ctx, m.now())
			if err != nil && ctx.Err() == nil {
				m.log.Warn("runner sweep", "err", err)
			}
			if res != (SweepResult{}) {
				m.log.Info("runner sweep", "offline", res.Offline, "requeued", res.Requeued, "failed", res.Failed, "stalled", res.Stalled)
			}
		}
	}
}

// --- helpers ---

func (m *Manager) tx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// emitJob writes a job event plus its task-level mirror so task and inbox views update.
func (m *Manager) emitJob(ctx context.Context, q *db.Queries, typ string, job db.Job, extra map[string]any) error {
	payload := map[string]any{"task_id": uuidString(job.TaskID), "status": job.Status, "phase": job.Phase, "role": job.Role, "backend": job.Backend}
	for k, v := range extra {
		payload[k] = v
	}
	if err := emit(ctx, q, typ, "job", job.ID, payload); err != nil {
		return err
	}
	taskPayload := map[string]any{"job_id": uuidString(job.ID), "status": job.Status}
	return emit(ctx, q, "task."+strings.ReplaceAll(typ, ".", "_"), "task", job.TaskID, taskPayload)
}

func emit(ctx context.Context, q *db.Queries, typ, aggregate string, id pgtype.UUID, payload map[string]any) error {
	b, _ := json.Marshal(payload)
	_, err := q.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: aggregate, AggregateID: id, Payload: b})
	return err
}

func terminal(status string) bool {
	return status == proto.StatusDone || status == proto.StatusFailed || status == proto.StatusStopped
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	v, _ := u.Value()
	s, _ := v.(string)
	return s
}

func parseUUID(s string) (pgtype.UUID, bool) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return u, false
	}
	return u, u.Valid
}

func runnersOnline(ctx context.Context, delta int64) {
	if m, err := telemetry.Instruments(); err == nil {
		m.RunnersOnline.Add(ctx, delta, metric.WithAttributes())
	}
}
