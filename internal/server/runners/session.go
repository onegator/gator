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
		s.handle(env)
	}
}

// handle answers one message from the runner.
func (s *session) handle(env proto.RawEnvelope) {
	switch env.Type {
	case proto.TypeHeartbeat:
		var hb proto.Heartbeat
		if err := env.Into(&hb); err != nil {
			s.sendError("bad_heartbeat", err)
			return
		}
		s.heartbeat(hb)
	case proto.TypeLeaseRequest:
		var lr proto.LeaseRequest
		_ = env.Into(&lr)
		if err := s.dispatch(s.ctx, lr.Slots, true); err != nil {
			s.m.log.Warn("dispatch", "runner", s.name, "err", err)
		}
	case proto.TypeEvents:
		report(s, env, "events", s.events)
	case proto.TypeFinish:
		if report(s, env, "finish", s.finish) {
			if err := s.dispatch(s.ctx, 0, false); err != nil {
				s.m.log.Warn("dispatch after finish", "runner", s.name, "err", err)
			}
		}
	case proto.TypeTurnEnding:
		report(s, env, "turn_ending", func(te proto.TurnEnding) error {
			s.turnEnding(te)
			return nil
		})
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

// report handles a message the runner keeps until it is acked. An unreadable one is acked
// with an error, because resending it will not make it readable; so is one handle refuses,
// unless the failure was ours (errRetry) and a resend may succeed. It says whether handle
// ran and its outcome was acked.
func report[T any](s *session, env proto.RawEnvelope, name string, handle func(T) error) bool {
	var v T
	if err := env.Into(&v); err != nil {
		s.sendError("bad_"+name, err)
		s.ack(env.Seq)
		return false
	}
	if err := handle(v); err != nil {
		s.sendError(name+"_rejected", err)
		if errors.Is(err, errRetry) {
			return false // not acked: the runner resends
		}
	}
	s.ack(env.Seq)
	return true
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
	free, err := s.freeSlots(ctx, q, requested)
	if err != nil {
		return err
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
			out = append(out, s.handOver(ctx, q, j))
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

// freeSlots is how many more jobs the runner can take, capped by what it asked for. A runner
// with no backends takes none.
func (s *session) freeSlots(ctx context.Context, q *db.Queries, requested int) (int, error) {
	if len(s.caps.Backends) == 0 {
		return 0, nil
	}
	active, err := q.CountActiveJobsForRunner(ctx, s.runnerID)
	if err != nil {
		return 0, err
	}
	free := s.caps.MaxParallel - int(active)
	if requested > 0 && requested < free {
		free = requested
	}
	return free, nil
}

// handOver is a leased job as the runner receives it: the task, the agent's identity and
// guide, the documents it starts from and the repo it works in. What cannot be found is left
// out rather than holding the job back.
func (s *session) handOver(ctx context.Context, q *db.Queries, j db.Job) proto.Job {
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
	pj.AgentToken = s.agentToken(ctx, j)
	if g, err := s.m.process.RoleGuide(ctx, j.ProjectID, j.Role); err == nil {
		pj.Guide = g
	}
	pj.Context = s.handContext(ctx, q, j)
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
	return pj
}

// agentToken is the identity the agent talks back with. Minted per job and revoked when the
// job ends, so it can only ever discuss the work it was handed. Empty without an issuer.
func (s *session) agentToken(ctx context.Context, j db.Job) string {
	issuer := s.m.cfg.AgentTokens
	if issuer == nil {
		return ""
	}
	ttl := s.m.cfg.AgentTokenTTL
	if ttl == 0 {
		ttl = 12 * time.Hour
	}
	token, err := issuer.IssueForJob(ctx, j.ID, j.ProjectID, ttl)
	if err != nil {
		s.m.log.Warn("minting the agent's identity", "job", uuidString(j.ID), "err", err)
		return ""
	}
	return token
}

// handContext gathers the documents the job starts from — the core's, then the plugins' —
// and records each one, whether or not it made it into the prompt.
func (s *session) handContext(ctx context.Context, q *db.Queries, j db.Job) []proto.ContextDoc {
	var out []proto.ContextDoc
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
				out = append(out, proto.ContextDoc{Kind: d.Kind, Phase: d.Phase, Title: d.Title, Body: d.Body})
			}
			record(d, d.Origin)
		}
	}
	if p := s.m.cfg.Preparer; p != nil {
		for _, d := range p.PrepareJob(ctx, j) {
			out = append(out, proto.ContextDoc{Kind: d.Kind, Phase: d.Phase, Title: d.Title, Body: d.Body})
			d.FullBytes = len(d.Body)
			record(d, "plugin")
		}
	}
	return out
}

// ownJob loads the job a runner reports on, refusing one it does not hold. A database
// failure is errRetry: the runner resends.
func (s *session) ownJob(ctx context.Context, q *db.Queries, id string) (db.Job, error) {
	jobID, ok := parseUUID(id)
	if !ok {
		return db.Job{}, fmt.Errorf("bad job id %q", id)
	}
	job, err := q.GetJob(ctx, jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Job{}, fmt.Errorf("unknown job %s", id)
	}
	if err != nil {
		return db.Job{}, errRetry
	}
	if job.RunnerID != s.runnerID {
		return db.Job{}, fmt.Errorf("job %s is not leased to this runner", id)
	}
	return job, nil
}

func (s *session) events(ev proto.Events) error {
	ctx := s.ctx
	q := db.New(s.m.pool)
	job, err := s.ownJob(ctx, q, ev.JobID)
	if err != nil {
		return err
	}
	jobID := job.ID
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
	job, err := s.ownJob(ctx, db.New(s.m.pool), fin.JobID)
	if err != nil {
		return err
	}
	if terminal(job.Status) {
		return nil // a resend after reconnect; already recorded
	}
	status, reason := verdict(fin)
	// Usage first: it is idempotent per job, so a crash before the finish commit loses nothing.
	if fin.Usage.Reported() {
		invalid, err := s.recordUsage(ctx, job, fin)
		if err != nil {
			return errRetry
		}
		if invalid != nil {
			status, reason = proto.StatusFailed, "invalid usage report: "+invalid.Error()
		}
	}
	// Whatever the agent was given stops working now, whichever way the job ended.
	if issuer := s.m.cfg.AgentTokens; issuer != nil {
		if err := issuer.RevokeForJob(ctx, job.ID); err != nil {
			s.m.log.Warn("revoking the agent's identity", "job", uuidString(job.ID), "err", err)
		}
	}
	if err := s.m.tx(ctx, func(q *db.Queries) error { return s.closeJob(ctx, q, job.ID, fin, status, reason) }); err != nil {
		return errRetry
	}
	return nil
}

// verdict is the status the job ends with. A runner's word is taken for failure and stops,
// but success has to come with a usage report, and a status nobody knows is a failure.
func verdict(fin proto.Finish) (status, reason string) {
	status, reason = fin.Status, fin.StopReason
	switch status {
	case proto.StatusDone, proto.StatusFailed, proto.StatusStopped:
	default:
		return proto.StatusFailed, fmt.Sprintf("runner reported unknown status %q", fin.Status)
	}
	if status == proto.StatusDone && !fin.Usage.Reported() {
		return proto.StatusFailed, "done without a usage report"
	}
	return status, reason
}

// recordUsage stores what the job spent. A report the core refuses comes back as invalid,
// which fails the job; any other error is the database's and the runner resends.
func (s *session) recordUsage(ctx context.Context, job db.Job, fin proto.Finish) (invalid, err error) {
	u := fin.Usage
	backend := u.Backend
	if backend == "" {
		backend = job.Backend
	}
	in := process.UsageInput{
		JobID: job.ID, Phase: job.Phase, Backend: backend, Model: u.Model,
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
	_, err = s.m.process.RecordUsage(ctx, job.TaskID, in, process.Actor{Kind: process.ActorRunner, ID: s.tokenID})
	if errors.Is(err, process.ErrInvalidUsage) {
		return err, nil
	}
	return nil, err
}

// closeJob records the job's end inside q's transaction, unless it already ended or was
// taken from this runner while it worked.
func (s *session) closeJob(ctx context.Context, q *db.Queries, jobID pgtype.UUID, fin proto.Finish, status, reason string) error {
	j, err := q.GetJobForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	if terminal(j.Status) || j.RunnerID != s.runnerID {
		return nil
	}
	receipt, _ := json.Marshal(fin)
	var stop *string
	if reason != "" {
		stop = &reason
	}
	if err := q.FinishJob(ctx, db.FinishJobParams{ID: jobID, Status: status, Receipt: receipt, StopReason: stop}); err != nil {
		return err
	}
	if err := q.InsertReceipt(ctx, db.InsertReceiptParams{Source: "runner", SubjectKind: "job", SubjectID: jobID, Status: status, Payload: receipt}); err != nil {
		return err
	}
	if status == proto.StatusDone {
		if err := recordWork(ctx, q, j, fin); err != nil {
			return err
		}
	}
	j.Status = status
	return s.m.emitJob(ctx, q, "job.finished", j, map[string]any{"stop_reason": reason, "summary": fin.Summary})
}

// recordWork leaves the document the job's role produces, for the phase gate to approve, and
// says whether the agent left a digest of what it did.
func recordWork(ctx context.Context, q *db.Queries, j db.Job, fin proto.Finish) error {
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
	return refreshWorkingState(ctx, q, j.TaskID)
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
		case "expired", "missing", "no_cli":
			// no_cli is not a login problem, but a runner without the command cannot run the
			// job either, so it is no more leasable than a logged-out one.
			continue
		}
		out = append(out, b)
	}
	return out
}
