package runners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// session is one connected runner.
type session struct {
	m        *Manager
	conn     *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	runnerID pgtype.UUID
	tokenID  pgtype.UUID
	name     string
	caps     proto.Capabilities
	authMu   sync.Mutex
	auth     map[string]string // backend → login state from the last heartbeat
	projects []pgtype.UUID
	seq      atomic.Uint64

	dispatchMu sync.Mutex
	closeOnce  sync.Once
}

func (m *Manager) serve(parent context.Context, c *websocket.Conn, p auth.Principal) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer c.CloseNow()

	reject := func(code, msg string) {
		b, _ := proto.Encode(proto.TypeError, 1, proto.Error{Code: code, Message: msg, Fatal: true})
		wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
		_ = c.Write(wctx, websocket.MessageText, b)
		wcancel()
		_ = c.Close(websocket.StatusPolicyViolation, code)
	}

	rctx, rcancel := context.WithTimeout(ctx, m.cfg.RegisterTimeout)
	_, first, err := c.Read(rctx)
	rcancel()
	if err != nil {
		return
	}
	env, err := proto.Decode(first)
	if err != nil || env.Type != proto.TypeRegister {
		reject("register_first", "the first message must be register")
		return
	}
	version, err := proto.Negotiate(env.Version)
	if err != nil {
		reject("unsupported_version", err.Error())
		return
	}
	var reg proto.Register
	if err := env.Into(&reg); err != nil {
		reject("bad_register", err.Error())
		return
	}
	if reg.Capabilities.MaxParallel < 1 {
		reg.Capabilities.MaxParallel = 1
	}
	if reg.Name == "" {
		reg.Name = p.Name
	}
	switch reg.Location {
	case "vps", "mac":
	default:
		reg.Location = "other"
	}
	var projects []pgtype.UUID
	for _, s := range reg.Capabilities.Projects {
		if u, ok := parseUUID(s); ok {
			projects = append(projects, u)
		}
	}
	capsJSON, _ := json.Marshal(reg.Capabilities)
	row, err := db.New(m.pool).UpsertRunner(ctx, db.UpsertRunnerParams{
		TokenID: p.TokenID, Name: reg.Name, Location: reg.Location, Capabilities: capsJSON,
		ProtocolVersion: int32(version), BinaryVersion: reg.BinaryVersion,
	})
	if err != nil {
		m.log.Error("runner upsert", "err", err)
		reject("internal", "could not register runner")
		return
	}

	s := &session{m: m, conn: c, runnerID: row.ID, tokenID: p.TokenID, name: reg.Name, caps: reg.Capabilities, projects: projects}
	s.ctx, s.cancel = ctx, cancel
	m.mu.Lock()
	if old := m.sessions[row.ID.Bytes]; old != nil {
		go old.close("replaced by a new connection")
	}
	m.sessions[row.ID.Bytes] = s
	m.mu.Unlock()

	if err := s.send(proto.TypeRegistered, proto.Registered{RunnerID: uuidString(row.ID), Version: version}); err != nil {
		m.dropSession(s)
		return
	}
	runnersOnline(ctx, 1)
	_ = m.tx(ctx, func(q *db.Queries) error {
		return emit(ctx, q, "runner.online", "runner", row.ID, map[string]any{"name": reg.Name, "location": reg.Location, "backends": reg.Capabilities.Backends})
	})
	m.log.Info("runner connected", "runner", reg.Name, "location", reg.Location, "backends", reg.Capabilities.Backends, "proto", version)

	s.readLoop()

	runnersOnline(context.Background(), -1)
	if m.dropSession(s) {
		bg := context.Background()
		_ = db.New(m.pool).SetRunnerStatus(bg, db.SetRunnerStatusParams{ID: row.ID, Status: "offline", Reason: "disconnected"})
		_ = m.tx(bg, func(q *db.Queries) error {
			return emit(bg, q, "runner.offline", "runner", row.ID, map[string]any{"name": reg.Name, "reason": "disconnected"})
		})
		m.log.Info("runner disconnected", "runner", reg.Name)
	}
}

// dropSession removes s if it is still the current session; reports whether it was.
func (m *Manager) dropSession(s *session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[s.runnerID.Bytes] == s {
		delete(m.sessions, s.runnerID.Bytes)
		return true
	}
	return false
}

func (s *session) close(reason string) {
	s.closeOnce.Do(func() {
		_ = s.conn.Close(websocket.StatusGoingAway, reason)
		s.cancel()
	})
}

// send writes one message. coder/websocket allows concurrent writers.
func (s *session) send(t proto.MessageType, payload any) error {
	b, err := proto.Encode(t, s.seq.Add(1), payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, b)
}

func (s *session) sendError(code string, err error) {
	_ = s.send(proto.TypeError, proto.Error{Code: code, Message: err.Error()})
}

func (s *session) ack(seq uint64) { _ = s.send(proto.TypeAck, proto.Ack{Seq: seq}) }

func (s *session) readLoop() {
	for {
		_, b, err := s.conn.Read(s.ctx)
		if err != nil {
			return
		}
		env, err := proto.Decode(b)
		if err != nil {
			s.sendError("bad_message", err)
			continue
		}
		switch env.Type {
		case proto.TypeHeartbeat:
			var hb proto.Heartbeat
			if err := env.Into(&hb); err != nil {
				s.sendError("bad_heartbeat", err)
				continue
			}
			s.heartbeat(hb)
		case proto.TypeLeaseRequest:
			var lr proto.LeaseRequest
			_ = env.Into(&lr)
			if err := s.dispatch(s.ctx, lr.Slots, true); err != nil {
				s.m.log.Warn("dispatch", "runner", s.name, "err", err)
			}
		case proto.TypeEvents:
			var ev proto.Events
			if err := env.Into(&ev); err != nil {
				s.sendError("bad_events", err)
				s.ack(env.Seq)
				continue
			}
			if err := s.events(ev); err != nil {
				s.sendError("events_rejected", err)
				if errors.Is(err, errRetry) {
					continue // not acked: the runner resends
				}
			}
			s.ack(env.Seq)
		case proto.TypeFinish:
			var fin proto.Finish
			if err := env.Into(&fin); err != nil {
				s.sendError("bad_finish", err)
				s.ack(env.Seq)
				continue
			}
			if err := s.finish(fin); err != nil {
				s.sendError("finish_rejected", err)
				if errors.Is(err, errRetry) {
					continue
				}
			}
			s.ack(env.Seq)
			if err := s.dispatch(s.ctx, 0, false); err != nil {
				s.m.log.Warn("dispatch after finish", "runner", s.name, "err", err)
			}
		case proto.TypeLoginPrompt:
			var lp proto.LoginPrompt
			if err := env.Into(&lp); err == nil {
				payload, _ := json.Marshal(lp)
				s.m.hub.Publish(events.Event{Type: "runner.login_prompt", Aggregate: "runner", AggregateID: uuidString(s.runnerID), Payload: payload, CreatedAt: time.Now()})
			}
		default:
			s.sendError("unknown_type", fmt.Errorf("unknown message type %q", env.Type))
		}
	}
}

// errRetry marks a transient server failure: the message is not acked so the runner resends.
var errRetry = errors.New("temporary failure; resend")

func (s *session) heartbeat(hb proto.Heartbeat) {
	ctx := s.ctx
	q := db.New(s.m.pool)
	status := "online"
	if len(hb.ActiveJobs) >= s.caps.MaxParallel {
		status = "busy"
	}
	s.authMu.Lock()
	s.auth = hb.AuthState
	s.authMu.Unlock()
	auth, _ := json.Marshal(hb.AuthState)
	if hb.AuthState == nil {
		auth = []byte("{}")
	}
	if err := q.RunnerHeartbeat(ctx, db.RunnerHeartbeatParams{ID: s.runnerID, AuthState: auth, Status: status}); err != nil {
		s.m.log.Warn("heartbeat", "runner", s.name, "err", err)
		return
	}
	ids := make([]pgtype.UUID, 0, len(hb.ActiveJobs))
	for _, id := range hb.ActiveJobs {
		if u, ok := parseUUID(id); ok {
			ids = append(ids, u)
		}
	}
	if len(ids) > 0 {
		if _, err := q.ExtendLeases(ctx, db.ExtendLeasesParams{LeaseUntil: ts(s.m.now().Add(s.m.cfg.LeaseTTL)), RunnerID: s.runnerID, JobIds: ids}); err != nil {
			s.m.log.Warn("extend leases", "runner", s.name, "err", err)
		}
	}
	if err := s.dispatch(ctx, 0, false); err != nil {
		s.m.log.Warn("dispatch on heartbeat", "runner", s.name, "err", err)
	}
}

// dispatch leases up to the runner's free slots (or `requested`, if lower) and sends them.
// With sendEmpty it answers even when nothing is queued, so a lease_request always gets a reply.
func (s *session) dispatch(ctx context.Context, requested int, sendEmpty bool) error {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	q := db.New(s.m.pool)
	free := 0
	if len(s.caps.Backends) > 0 {
		active, err := q.CountActiveJobsForRunner(ctx, s.runnerID)
		if err != nil {
			return err
		}
		free = s.caps.MaxParallel - int(active)
		if requested > 0 && requested < free {
			free = requested
		}
	}
	var out []proto.Job
	backends := s.leasable()
	if free > 0 && len(backends) > 0 {
		projects := s.projects
		if projects == nil {
			projects = []pgtype.UUID{}
		}
		jobs, err := q.LeaseJobs(ctx, db.LeaseJobsParams{
			RunnerID: s.runnerID, LeaseUntil: ts(s.m.now().Add(s.m.cfg.LeaseTTL)),
			Backends: backends, Projects: projects, Slots: int32(free),
		})
		if err != nil {
			return err
		}
		for _, j := range jobs {
			var b proto.Bounds
			_ = json.Unmarshal(j.Bounds, &b)
			pj := proto.Job{
				JobID: uuidString(j.ID), TaskID: uuidString(j.TaskID), ProjectID: uuidString(j.ProjectID),
				Phase: j.Phase, Role: j.Role, Backend: j.Backend, Model: j.Model, Instruction: j.Instruction, Bounds: b,
				Attempt: int(j.Attempts), LeaseExpiresAt: j.LeaseExpiresAt.Time,
			}
			if t, err := q.GetTask(ctx, j.TaskID); err == nil {
				pj.TaskTitle, pj.TaskDescription, pj.TaskOrigin = t.Title, t.Description, t.Origin
			}
			// The identity the agent talks back with. Minted per job and revoked when the job
			// ends, so it can only ever discuss the work it was handed.
			if issuer := s.m.cfg.AgentTokens; issuer != nil {
				ttl := s.m.cfg.AgentTokenTTL
				if ttl == 0 {
					ttl = 12 * time.Hour
				}
				if token, err := issuer.IssueForJob(ctx, j.ID, j.ProjectID, ttl); err == nil {
					pj.AgentToken = token
				} else {
					s.m.log.Warn("minting the agent's identity", "job", uuidString(j.ID), "err", err)
				}
			}
			if g, err := s.m.process.RoleGuide(ctx, j.ProjectID, j.Role); err == nil {
				pj.Guide = g
			}
			// One counter across everything the job is handed, so the record reads in the
			// order the prompt does.
			pos := 0
			record := func(d process.ContextDoc, origin string) {
				if err := q.SaveJobContextDoc(ctx, db.SaveJobContextDocParams{
					JobID: j.ID, Position: int32(pos), Kind: d.Kind, Phase: d.Phase, Title: d.Title,
					Origin: origin, Body: d.Body, FullBytes: int32(d.FullBytes),
					LeftOut: d.Left, Dropped: d.Dropped}); err != nil {
					s.m.log.Warn("recording what a job was handed", "job", uuidString(j.ID), "err", err)
				}
				pos++
			}
			if docs, err := s.m.process.JobContext(ctx, j.TaskID, j.Role); err == nil {
				for _, d := range docs {
					// A document that did not fit is kept in the record but not in the prompt:
					// the agent never saw it, and the record should say so rather than imply
					// it was never there.
					if !d.Dropped {
						pj.Context = append(pj.Context, proto.ContextDoc{Kind: d.Kind, Phase: d.Phase, Title: d.Title, Body: d.Body})
					}
					record(d, d.Origin)
				}
			}
			if p := s.m.cfg.Preparer; p != nil {
				for _, d := range p.PrepareJob(ctx, j) {
					pj.Context = append(pj.Context, proto.ContextDoc{Kind: d.Kind, Phase: d.Phase, Title: d.Title, Body: d.Body})
					d.FullBytes = len(d.Body)
					record(d, "plugin")
				}
			}
			if r, err := q.GetPrimaryRepo(ctx, j.ProjectID); err == nil {
				pj.Repo = &proto.Repo{Name: r.Name, URL: r.Url, DefaultBranch: r.DefaultBranch}
			}
			// Record what this job was handed. Context is meant to save an agent from
			// hunting for what it needs; whether it does is a question about tokens spent
			// against context given, and that comparison needs both numbers.
			bytes := 0
			for _, d := range pj.Context {
				bytes += len(d.Body)
			}
			_ = q.SetJobContextSize(ctx, db.SetJobContextSizeParams{
				ID: j.ID, ContextBytes: int32(bytes), ContextDocs: int32(len(pj.Context))})
			out = append(out, pj)
			_ = s.m.tx(ctx, func(q *db.Queries) error {
				return s.m.emitJob(ctx, q, "job.leased", j, map[string]any{"runner": s.name, "attempt": j.Attempts})
			})
		}
	}
	if len(out) == 0 && !sendEmpty {
		return nil
	}
	if out == nil {
		out = []proto.Job{}
	}
	// If this send fails the leases expire and the sweeper returns the jobs to the queue.
	return s.send(proto.TypeLease, proto.Lease{Jobs: out})
}

func (s *session) events(ev proto.Events) error {
	ctx := s.ctx
	jobID, ok := parseUUID(ev.JobID)
	if !ok {
		return fmt.Errorf("bad job id %q", ev.JobID)
	}
	q := db.New(s.m.pool)
	job, err := q.GetJob(ctx, jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("unknown job %s", ev.JobID)
	}
	if err != nil {
		return errRetry
	}
	if job.RunnerID != s.runnerID {
		return fmt.Errorf("job %s is not leased to this runner", ev.JobID)
	}
	prev := job.Status
	if _, err := q.MarkJobActive(ctx, db.MarkJobActiveParams{ID: jobID, RunnerID: s.runnerID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("job %s is %s, not active", ev.JobID, job.Status)
		}
		return errRetry
	}
	if prev == "leased" || prev == "stalled" {
		typ := "job.started"
		if prev == "stalled" {
			typ = "job.resumed"
		}
		job.Status = "running"
		_ = s.m.tx(ctx, func(q *db.Queries) error { return s.m.emitJob(ctx, q, typ, job, nil) })
	}
	for _, e := range ev.Events {
		payload := []byte(e.Payload)
		if len(payload) == 0 {
			payload = []byte("{}")
		}
		at := e.At
		if at.IsZero() {
			at = s.m.now()
		}
		n, err := q.InsertJobEvent(ctx, db.InsertJobEventParams{JobID: jobID, Seq: int64(e.Seq), Type: e.Type, Payload: payload, At: ts(at)})
		if err != nil {
			return errRetry
		}
		if n == 1 {
			out, _ := json.Marshal(e)
			s.m.hub.Publish(events.Event{Type: "job.output", Aggregate: "job", AggregateID: ev.JobID, Payload: out, CreatedAt: at})
		}
	}
	return nil
}

func (s *session) finish(fin proto.Finish) error {
	ctx := s.ctx
	jobID, ok := parseUUID(fin.JobID)
	if !ok {
		return fmt.Errorf("bad job id %q", fin.JobID)
	}
	q := db.New(s.m.pool)
	job, err := q.GetJob(ctx, jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("unknown job %s", fin.JobID)
	}
	if err != nil {
		return errRetry
	}
	if job.RunnerID != s.runnerID {
		return fmt.Errorf("job %s is not leased to this runner", fin.JobID)
	}
	if terminal(job.Status) {
		return nil // a resend after reconnect; already recorded
	}

	status, reason := fin.Status, fin.StopReason
	switch status {
	case proto.StatusDone, proto.StatusFailed, proto.StatusStopped:
	default:
		status, reason = proto.StatusFailed, fmt.Sprintf("runner reported unknown status %q", fin.Status)
	}
	if status == proto.StatusDone && !fin.Usage.Reported() {
		status, reason = proto.StatusFailed, "done without a usage report"
	}

	// Usage first: it is idempotent per job, so a crash before the finish commit loses nothing.
	if fin.Usage.Reported() {
		u := fin.Usage
		backend := u.Backend
		if backend == "" {
			backend = job.Backend
		}
		in := process.UsageInput{
			JobID: jobID, Phase: job.Phase, Backend: backend, Model: u.Model,
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
			CostUSD: u.CostUSD, CostEstimated: u.CostEstimated, DurationMS: u.DurationMS,
			IdempotencyKey: "job:" + fin.JobID,
		}
		if !u.StartedAt.IsZero() {
			in.StartedAt = ts(u.StartedAt)
		}
		if !u.FinishedAt.IsZero() {
			in.FinishedAt = ts(u.FinishedAt)
		}
		if _, err := s.m.process.RecordUsage(ctx, job.TaskID, in, process.Actor{Kind: process.ActorRunner, ID: s.tokenID}); err != nil {
			if errors.Is(err, process.ErrInvalidUsage) {
				status, reason = proto.StatusFailed, "invalid usage report: "+err.Error()
			} else {
				return errRetry
			}
		}
	}

	receipt, _ := json.Marshal(fin)
	var stop *string
	if reason != "" {
		stop = &reason
	}
	// Whatever the agent was given stops working now, whichever way the job ended.
	if issuer := s.m.cfg.AgentTokens; issuer != nil {
		if err := issuer.RevokeForJob(ctx, jobID); err != nil {
			s.m.log.Warn("revoking the agent's identity", "job", uuidString(jobID), "err", err)
		}
	}
	err = s.m.tx(ctx, func(q *db.Queries) error {
		j, err := q.GetJobForUpdate(ctx, jobID)
		if err != nil {
			return err
		}
		if terminal(j.Status) || j.RunnerID != s.runnerID {
			return nil
		}
		if err := q.FinishJob(ctx, db.FinishJobParams{ID: jobID, Status: status, Receipt: receipt, StopReason: stop}); err != nil {
			return err
		}
		if err := q.InsertReceipt(ctx, db.InsertReceiptParams{Source: "runner", SubjectKind: "job", SubjectID: jobID, Status: status, Payload: receipt}); err != nil {
			return err
		}
		// A finished job leaves the document its role produces; the phase gate approves it.
		if status == proto.StatusDone {
			content := artifactContent(j.Role, fin)
			typ := process.ArtifactTypeFor(j.Role)
			a, err := q.CreateArtifact(ctx, db.CreateArtifactParams{TaskID: j.TaskID, Phase: j.Phase, Type: typ, Content: &content})
			if err != nil {
				return err
			}
			if err := emit(ctx, q, "artifact.created", "task", j.TaskID, map[string]any{"phase": j.Phase, "type": typ, "version": a.Version, "job_id": fin.JobID}); err != nil {
				return err
			}
			if fin.Digest != nil {
				if err := emit(ctx, q, "job.digest", "job", j.ID, map[string]any{"task_id": uuidString(j.TaskID), "phase": j.Phase, "role": j.Role, "digest": fin.Digest}); err != nil {
					return err
				}
			} else if err := emit(ctx, q, "job.digest_missing", "job", j.ID, map[string]any{"task_id": uuidString(j.TaskID)}); err != nil {
				return err
			}
			if err := refreshWorkingState(ctx, q, j.TaskID); err != nil {
				return err
			}
		}
		j.Status = status
		return s.m.emitJob(ctx, q, "job.finished", j, map[string]any{"stop_reason": reason, "summary": fin.Summary})
	})
	if err != nil {
		return errRetry
	}
	return nil
}

// artifactContent is the agent's final answer, plus the branch and commits for code work.
func artifactContent(role string, fin proto.Finish) string {
	var b strings.Builder
	summary := strings.TrimSpace(fin.Summary)
	if summary == "" {
		summary = "_The agent finished without a written summary._"
	}
	b.WriteString(summary)
	if fin.Branch != "" || len(fin.Commits) > 0 {
		fmt.Fprintf(&b, "\n\n---\nBranch `%s`, %d commit(s), %d file(s) changed", fin.Branch, len(fin.Commits), fin.ChangedFiles)
		for _, c := range fin.Commits {
			short := c
			if len(short) > 12 {
				short = short[:12]
			}
			fmt.Fprintf(&b, "\n- `%s`", short)
		}
	}
	_ = role
	return b.String()
}

// leasable is the runner's backends minus those it reports as logged out: the job waits for
// a logged-in backend instead of running on another one.
func (s *session) leasable() []string {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	out := make([]string, 0, len(s.caps.Backends))
	for _, b := range s.caps.Backends {
		switch s.auth[b] {
		case "expired", "missing":
			continue
		}
		out = append(out, b)
	}
	return out
}
