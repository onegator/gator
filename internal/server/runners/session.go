package runners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/redact"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/brain"
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
	// placement is where this runner takes work from; owner is the person whose machine it is,
	// when it has one; onlyJob is set for a runner started for one job, which takes nothing else.
	placement string
	owner     pgtype.UUID
	onlyJob   pgtype.UUID
	// version is the protocol spoken on this connection.
	version int
	seq     atomic.Uint64

	dispatchMu sync.Mutex
	closeOnce  sync.Once

	// masks are the values of the logins this runner's jobs were handed, by job: what its reports
	// are masked of before they are kept or shown.
	masksMu sync.Mutex
	masks   map[[16]byte][]string
}

// remember keeps the values a job's reports are masked of.
func (s *session) remember(job pgtype.UUID, values []string) {
	s.masksMu.Lock()
	defer s.masksMu.Unlock()
	if s.masks == nil {
		s.masks = map[[16]byte][]string{}
	}
	s.masks[job.Bytes] = values
}

// masksOf are the values j's reports are masked of: remembered from its lease, or — after the
// server restarted, or the job moved here from another connection — read from its login again.
func (s *session) masksOf(ctx context.Context, j db.Job) []string {
	if !j.CredentialID.Valid {
		return nil
	}
	s.masksMu.Lock()
	values, ok := s.masks[j.ID.Bytes]
	s.masksMu.Unlock()
	if ok {
		return values
	}
	values = s.m.cfg.Credentials.Sensitive(ctx, j)
	s.remember(j.ID, values)
	return values
}

func (s *session) forget(job pgtype.UUID) {
	s.masksMu.Lock()
	defer s.masksMu.Unlock()
	delete(s.masks, job.Bytes)
}

func (m *Manager) serve(parent context.Context, c *websocket.Conn, p auth.Principal) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer c.CloseNow()
	reg, version, ok := m.handshake(ctx, c, p)
	if !ok {
		return
	}
	s, ok := m.connect(ctx, cancel, c, p, reg, version)
	if !ok {
		return
	}
	s.readLoop()
	m.disconnect(s)
}

// reject tells the runner why it is refused and closes the connection.
func reject(ctx context.Context, c *websocket.Conn, code, msg string) {
	b, _ := proto.Encode(proto.TypeError, 1, proto.Error{Code: code, Message: msg, Fatal: true})
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	_ = c.Write(wctx, websocket.MessageText, b)
	wcancel()
	_ = c.Close(websocket.StatusPolicyViolation, code)
}

// handshake reads the runner's register, which must come first and in a protocol version
// this server speaks, and fills in what the runner left out.
func (m *Manager) handshake(ctx context.Context, c *websocket.Conn, p auth.Principal) (proto.Register, int, bool) {
	rctx, rcancel := context.WithTimeout(ctx, m.cfg.RegisterTimeout)
	_, first, err := c.Read(rctx)
	rcancel()
	if err != nil {
		return proto.Register{}, 0, false
	}
	env, err := proto.Decode(first)
	if err != nil || env.Type != proto.TypeRegister {
		reject(ctx, c, "register_first", "the first message must be register")
		return proto.Register{}, 0, false
	}
	version, err := proto.Negotiate(env.Version)
	if err != nil {
		reject(ctx, c, "unsupported_version", err.Error())
		return proto.Register{}, 0, false
	}
	var reg proto.Register
	if err := env.Into(&reg); err != nil {
		reject(ctx, c, "bad_register", err.Error())
		return proto.Register{}, 0, false
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
	switch {
	case p.JobID.Valid:
		// A token minted for one job belongs to a runner the server started in the cloud.
		reg.Placement = proto.PlacementCloud
	case reg.Placement == "":
		reg.Placement = proto.PlacementFor(reg.Location)
	case !proto.ValidPlacement(reg.Placement):
		reject(ctx, c, "bad_register", fmt.Sprintf("placement must be local or cloud, not %q", reg.Placement))
		return proto.Register{}, 0, false
	}
	// A project the server cannot read used to be dropped, and a list of only such entries
	// became "any project" — the opposite of what its author meant. Refuse it instead.
	for _, id := range reg.Capabilities.Projects {
		if _, ok := parseUUID(id); !ok {
			reject(ctx, c, "bad_register", fmt.Sprintf("project %q is not a project id; GATOR_RUNNER_PROJECTS takes ids", id))
			return proto.Register{}, 0, false
		}
	}
	if p.ProjectID.Valid {
		reg.Capabilities.Projects = []string{uuidString(p.ProjectID)}
	}
	return reg, version, true
}

// connect records the runner, makes this connection its session — replacing one it may still
// have open — and tells the runner and everyone watching that it is online.
func (m *Manager) connect(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, p auth.Principal, reg proto.Register, version int) (*session, bool) {
	var projects []pgtype.UUID
	for _, s := range reg.Capabilities.Projects {
		if u, ok := parseUUID(s); ok {
			projects = append(projects, u)
		}
	}
	capsJSON, _ := json.Marshal(reg.Capabilities)
	m.statusMu.Lock()
	row, err := db.New(m.pool).UpsertRunner(ctx, db.UpsertRunnerParams{
		TokenID: p.TokenID, Name: reg.Name, Location: reg.Location, Capabilities: capsJSON,
		ProtocolVersion: int32(version), BinaryVersion: reg.BinaryVersion, Placement: reg.Placement,
	})
	if err != nil {
		m.statusMu.Unlock()
		m.log.Error("runner upsert", "err", err)
		reject(ctx, c, "internal", "could not register runner")
		return nil, false
	}

	s := &session{m: m, conn: c, runnerID: row.ID, tokenID: p.TokenID, name: reg.Name, caps: reg.Capabilities, projects: projects,
		placement: reg.Placement, owner: row.OwnerID, onlyJob: p.JobID, version: version}
	s.ctx, s.cancel = ctx, cancel
	m.mu.Lock()
	if old := m.sessions[row.ID.Bytes]; old != nil {
		go old.close("replaced by a new connection")
	}
	m.sessions[row.ID.Bytes] = s
	m.mu.Unlock()
	m.statusMu.Unlock()

	if err := s.send(proto.TypeRegistered, proto.Registered{RunnerID: uuidString(row.ID), Version: version}); err != nil {
		m.dropSession(s)
		return nil, false
	}
	runnersOnline(ctx, 1)
	_ = m.tx(ctx, func(q *db.Queries) error {
		return emit(ctx, q, "runner.online", "runner", row.ID, map[string]any{"name": reg.Name, "location": reg.Location,
			"placement": reg.Placement, "backends": reg.Capabilities.Backends})
	})
	m.log.Info("runner connected", "runner", reg.Name, "location", reg.Location, "placement", reg.Placement,
		"backends", reg.Capabilities.Backends, "proto", version)
	return s, true
}

// disconnect marks the runner offline, unless a newer connection has already taken its place.
func (m *Manager) disconnect(s *session) {
	runnersOnline(context.Background(), -1)
	bg := context.Background()
	m.statusMu.Lock()
	if !m.dropSession(s) {
		m.statusMu.Unlock()
		return
	}
	_ = db.New(m.pool).SetRunnerStatus(bg, db.SetRunnerStatusParams{ID: s.runnerID, Status: "offline", Reason: "disconnected"})
	m.statusMu.Unlock()
	_ = m.tx(bg, func(q *db.Queries) error {
		return emit(bg, q, "runner.offline", "runner", s.runnerID, map[string]any{"name": s.name, "reason": "disconnected"})
	})
	m.log.Info("runner disconnected", "runner", s.name)
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
	changed := !maps.Equal(s.auth, hb.AuthState)
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
	if changed {
		// A backend that lost its login is a decision in the inbox; say so the moment it is
		// known rather than when somebody next reloads.
		_ = s.m.tx(ctx, func(q *db.Queries) error {
			return emit(ctx, q, "runner.auth_changed", "runner", s.runnerID, map[string]any{"auth": hb.AuthState})
		})
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
	if free > 0 {
		leased, err := s.lease(ctx, free)
		if err != nil {
			return err
		}
		for _, l := range leased {
			out = append(out, s.handOver(ctx, q, l.job, l.credential))
			_ = s.m.tx(ctx, func(q *db.Queries) error {
				return s.m.emitJob(ctx, q, "job.leased", l.job, map[string]any{"runner": s.name, "attempt": l.job.Attempts,
					"credential_kind": l.job.CredentialKind})
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

// leasedJob is a job leased to the runner and the payer's login it runs on (nil: the runner's).
type leasedJob struct {
	job        db.Job
	credential *proto.Credential
}

// candidateSlack is how many queued jobs past its free slots a lease looks at, so that jobs
// waiting for a login do not keep a runner idle while others it could run sit behind them.
const candidateSlack = 20

// lease hands the runner up to free jobs, each with the login it runs on, in one transaction that
// holds the lease lock: a login one job at a time may use is never handed out twice. A job whose
// payer cannot pay waits and says why; a job on the runner's own login goes only to a runner
// logged in to its backend, and a job on its payer's only to a runner that says it runs payers'
// logins.
func (s *session) lease(ctx context.Context, free int) ([]leasedJob, error) {
	backends := s.runnable()
	if len(backends) == 0 {
		return nil, nil
	}
	loggedIn := s.leasable()
	projects := s.projects
	if projects == nil {
		projects = []pgtype.UUID{}
	}
	var out []leasedJob
	err := s.m.tx(ctx, func(q *db.Queries) error {
		out = out[:0]
		if err := q.LockLeases(ctx); err != nil {
			return err
		}
		domains, err := q.ActiveTrustDomains(ctx, s.runnerID)
		if err != nil {
			return err
		}
		jobs, err := q.LeaseCandidates(ctx, db.LeaseCandidatesParams{
			Backends: backends, Projects: projects, Slots: int32(free + candidateSlack),
			Placement: s.placementOrLocal(), OnlyJob: s.onlyJob, Owner: s.owner,
			PhaseOnly: s.version < turnsVersion,
			ReadOnly:  keeping(backends, s.caps.ReadOnly), NoTools: keeping(backends, s.caps.NoTools),
		})
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if len(out) >= free {
				break
			}
			payerLogins := slices.Contains(s.caps.PayerLogins, j.Backend)
			choice, err := s.m.cfg.Credentials.Choose(ctx, q, j, payerLogins)
			if err != nil {
				return err
			}
			switch {
			case choice.Wait != "":
				if j.WaitingReason != choice.Wait {
					if err := q.SetJobWaiting(ctx, db.SetJobWaitingParams{ID: j.ID, WaitingReason: choice.Wait}); err != nil {
						return err
					}
					if err := s.m.emitJob(ctx, q, "job.waiting", j, map[string]any{"waiting_reason": choice.Wait}); err != nil {
						return err
					}
				}
				continue
			case choice.Runner():
				if !slices.Contains(loggedIn, j.Backend) {
					continue
				}
			case s.owner.Valid && j.PayerID != s.owner:
				continue // a person's login goes to their own machine or a shared runner, never another's
			case !payerLogins:
				continue // it would run on this runner's own login
			}
			domain := trustDomain(choice, j)
			if !sharesTrust(domains, domain) {
				continue // another payer's login is on this runner now; this one waits for its own slot
			}
			domains = append(domains, domain)
			leased, err := q.LeaseJob(ctx, db.LeaseJobParams{ID: j.ID, RunnerID: s.runnerID, LeaseUntil: ts(s.m.now().Add(s.m.cfg.LeaseTTL)),
				CredentialID: choice.ID, CredentialKind: choice.Kind, CredentialExclusive: choice.Exclusive, CredentialVersion: choice.Version})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			out = append(out, leasedJob{job: leased, credential: choice.Credential})
		}
		return nil
	})
	return out, err
}

// turnsVersion is the protocol that knows a job's kind, tier and agent; a runner below it only
// runs phase jobs.
const turnsVersion = 3

// placementOrLocal is the session's placement; a session built without one (a test, an old
// row) is local, which is what every runner was before placement existed.
func (s *session) placementOrLocal() string {
	if s.placement == "" {
		return proto.PlacementLocal
	}
	return s.placement
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
func (s *session) handOver(ctx context.Context, q *db.Queries, j db.Job, login *proto.Credential) proto.Job {
	var b proto.Bounds
	_ = json.Unmarshal(j.Bounds, &b)
	pj := proto.Job{
		JobID: uuidString(j.ID), TaskID: uuidString(j.TaskID), ProjectID: uuidString(j.ProjectID),
		Phase: j.Phase, Role: j.Role, Backend: j.Backend, Model: j.Model, Instruction: j.Instruction, Bounds: b,
		Attempt: int(j.Attempts), LeaseExpiresAt: j.LeaseExpiresAt.Time, Credential: login,
	}
	if login != nil {
		s.remember(j.ID, login.Redact)
	}
	if s.version >= turnsVersion {
		pj.Kind, pj.Tier = j.Kind, j.Tier
		if j.AgentID.Valid {
			if a, err := q.GetAgent(ctx, j.AgentID); err == nil {
				pj.Agent = &proto.Agent{Handle: a.Handle, DisplayName: a.DisplayName}
				pj.Skills = s.skills(ctx, q, j, a)
				pj.MCP = s.proxied(ctx, q, j, a)
			}
		}
	}
	if t, err := q.GetTask(ctx, j.TaskID); err == nil {
		pj.TaskTitle, pj.TaskDescription, pj.TaskOrigin = t.Title, t.Description, t.Origin
	}
	pj.AgentToken = s.agentToken(ctx, j)
	pj.Guide = s.guide(ctx, q, j)
	brief := s.brief(ctx, j)
	pj.Context = s.handContext(ctx, q, j, brief)
	if j.Kind == KindReply && j.TurnID.Valid {
		pj.Turn = &proto.Turn{ID: uuidString(j.TurnID), Ask: brief.Ask, Repeat: brief.Repeat}
		for _, d := range brief.Docs {
			pj.Turn.Conversation = append(pj.Turn.Conversation, proto.ContextDoc{Kind: d.Kind, Phase: d.Phase, Title: d.Title, Body: d.Body})
		}
	}
	if r, err := q.GetPrimaryRepo(ctx, j.ProjectID); err == nil {
		pj.Repo = &proto.Repo{Name: r.Name, URL: r.Url, DefaultBranch: r.DefaultBranch}
		if j.Tier == TierChat {
			// A reviewer asked about a task reads the task's work, not the default branch.
			if b, err := q.TaskWorkBranch(ctx, j.TaskID); err == nil {
				pj.Repo.ReadBranch = b
			}
		}
	}
	// Record what this job was handed. Context is meant to save an agent from
	// hunting for what it needs; whether it does is a question about tokens spent
	// against context given, and that comparison needs both numbers.
	bytes := len(brief.Ask) + len(brief.Repeat)
	for _, d := range brief.Docs {
		bytes += len(d.Body)
	}
	for _, d := range pj.Context {
		bytes += len(d.Body)
	}
	_ = q.SetJobContextSize(ctx, db.SetJobContextSizeParams{
		ID: j.ID, ContextBytes: int32(bytes), ContextDocs: int32(len(pj.Context) + len(brief.Docs))})
	return pj
}

// guide is the prompt of the job's agent: its own instructions when a person wrote them, else
// the project's prompt for the role it plays, else that of the job's role.
func (s *session) guide(ctx context.Context, q *db.Queries, j db.Job) string {
	role := j.Role
	if j.AgentID.Valid {
		if a, err := q.GetAgent(ctx, j.AgentID); err == nil {
			if strings.TrimSpace(a.Instructions) != "" {
				return a.Instructions
			}
			if a.RoleKey != nil && *a.RoleKey != "" {
				role = *a.RoleKey
			}
		}
	}
	g, err := s.m.process.RoleGuide(ctx, j.ProjectID, role)
	if err != nil {
		s.m.log.Warn("reading the role's prompt", "job", uuidString(j.ID), "role", role, "err", err)
	}
	return g
}

// agentToken is the identity the agent talks back with. Minted per job and revoked when the
// job ends, so it can only ever discuss the work it was handed. Empty without an issuer.
func (s *session) agentToken(ctx context.Context, j db.Job) string {
	issuer := s.m.cfg.AgentTokens
	if issuer == nil || j.Kind == KindSummary { // a summary reads a transcript; it has nobody to talk to
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

// privateLeft is what the record of a job says of a private conversation it was handed.
const privateLeft = "said in a private conversation: handed to the agent, not kept in the job's record"

// Of a person's own context the record keeps neither the words nor the title.
const (
	personalTitle = "Private context of the person who asked"
	personalLeft  = "a person's own context: handed to the agent of their turn, not kept in the job's record"
)

// brief is the conversation a reply answers in, as its agent is told it; nothing for any other
// job, or when it cannot be told.
func (s *session) brief(ctx context.Context, j db.Job) TurnBrief {
	if j.Kind != KindReply || s.m.cfg.Briefer == nil {
		return TurnBrief{}
	}
	b, err := s.m.cfg.Briefer.Brief(ctx, j)
	if err != nil {
		s.m.log.Warn("telling an agent its conversation", "job", uuidString(j.ID), "err", err)
	}
	return b
}

// handContext gathers the documents the job starts from — the conversation a reply answers in,
// then the core's, then the plugins' — and records each one, whether or not it made it into the
// prompt.
func (s *session) handContext(ctx context.Context, q *db.Queries, j db.Job, brief TurnBrief) []proto.ContextDoc {
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
	for _, d := range brief.Docs {
		d.FullBytes = max(d.FullBytes, len(d.Body))
		if brief.Private {
			d.Body, d.Left = "", privateLeft
		}
		record(d, "conversation")
	}
	// A summary reads the transcript in its instruction and nothing else; it is handed no package.
	var docs []process.ContextDoc
	var err error
	if j.Kind != KindSummary {
		docs, err = s.m.process.ContextFor(ctx, s.contextRequest(ctx, q, j))
	}
	if err == nil && len(docs) > 0 {
		var packed []pgtype.UUID
		for _, d := range docs {
			// A document that did not fit is kept in the record but not in the prompt:
			// the agent never saw it, and the record should say so rather than imply
			// it was never there.
			if !d.Dropped {
				out = append(out, proto.ContextDoc{Kind: d.Kind, Phase: d.Phase, Title: d.Title, Body: d.Body})
				if d.Entry.Valid {
					packed = append(packed, d.Entry)
				}
			}
			if d.Private {
				d.Title, d.Body, d.Left = personalTitle, "", personalLeft
			}
			record(d, d.Origin)
		}
		if use := s.m.cfg.Brain; use != nil {
			if err := use.Used(ctx, j.ID, brain.HowPacked, packed); err != nil {
				s.m.log.Warn("recording what a job was handed from the Brain", "job", uuidString(j.ID), "err", err)
			}
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

// contextRequest is what j's documents are gathered for: its role and agent, and the person its
// turn acts for, whose private context it is lent.
func (s *session) contextRequest(ctx context.Context, q *db.Queries, j db.Job) process.ContextRequest {
	req := process.ContextRequest{Task: j.TaskID, Role: j.Role, Job: j.ID, Agent: j.AgentID}
	if j.TurnID.Valid {
		if turn, err := q.GetTurnByJob(ctx, j.ID); err == nil {
			req.OnBehalfOf = turn.OnBehalfOf
		}
	}
	return req
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
	masks := s.masksOf(ctx, job)
	for _, e := range ev.Events {
		payload := []byte(e.Payload)
		if len(payload) == 0 {
			payload = []byte("{}")
		}
		// The runner masks the login's values too; this is the server not trusting that it did.
		if len(masks) > 0 {
			payload = []byte(redact.Values(string(payload), masks))
			e.Payload = payload
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
	if err := s.keepLogin(ctx, job, &fin); err != nil {
		return err
	}
	if terminal(job.Status) {
		return nil // a resend after reconnect; already recorded
	}
	status, reason := verdict(fin)
	if fin.CredentialRefused != "" && job.CredentialID.Valid {
		marked, err := s.m.cfg.Credentials.Refused(ctx, job, fin.CredentialRefused)
		if err != nil {
			return errRetry
		}
		if marked {
			status, reason = proto.StatusFailed, "the model login of the person who pays for this job was turned down ("+
				fin.CredentialRefused+"); they can connect it again in Settings → Model logins"
		}
	}
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
	// What the agent says it relied on, of what it was handed or looked up.
	if use := s.m.cfg.Brain; use != nil && status == proto.StatusDone {
		if err := use.Cited(ctx, job.ID, fin.Summary); err != nil {
			s.m.log.Warn("recording what a job cited from the Brain", "job", uuidString(job.ID), "err", err)
		}
	}
	s.forget(job.ID)
	if job.Placement == proto.PlacementCloud {
		s.m.provisionSoon() // the machine that ran it goes now, not on the next sweep
	}
	if job.CredentialExclusive {
		s.m.Offer() // a login one job at a time may use is free for the next
	}
	return nil
}

// keepLogin stores what the job's agent rewrote of its login, before anything else of the finish
// — the old one may have stopped working when it was refreshed, so a receipt the server refuses
// must not lose it — and masks the login's values in what the finish says. What was rewritten is
// taken out of the finish: it is kept with the login, never in the job's receipt.
func (s *session) keepLogin(ctx context.Context, job db.Job, fin *proto.Finish) error {
	masks := s.masksOf(ctx, job)
	if r := fin.CredentialRefresh; r != nil {
		if err := s.m.cfg.Credentials.Refreshed(ctx, job, *r); err != nil {
			s.m.log.Warn("keeping what a job rewrote of its login", "job", uuidString(job.ID), "err", err)
			return errRetry
		}
		fin.CredentialRefresh = nil
		// The rotated values are masked as well as those the job was leased with.
		masks = redact.Union(masks, s.m.cfg.Credentials.Sensitive(ctx, job))
		s.remember(job.ID, masks)
	}
	if len(masks) > 0 {
		fin.Summary = redact.Values(fin.Summary, masks)
		fin.StopReason = redact.Values(fin.StopReason, masks)
		fin.CredentialRefused = redact.Values(fin.CredentialRefused, masks)
		if fin.Digest != nil {
			for _, l := range [][]string{fin.Digest.Changes, fin.Digest.Decisions, fin.Digest.Rejected, fin.Digest.Left} {
				for i := range l {
					l[i] = redact.Values(l[i], masks)
				}
			}
		}
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
		IdempotencyKey: "job:" + fin.JobID, CredentialKind: job.CredentialKind,
	}
	if job.CredentialKind != "" && job.CredentialKind != RunnerLogin {
		in.PaidBy = job.PayerID // whose own login paid for it
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
	extra := map[string]any{"stop_reason": reason}
	if j.Kind == KindPhase {
		extra["summary"] = fin.Summary // a reply's answer is its message, read through the conversation's rules
	}
	return s.m.emitJob(ctx, q, "job.finished", j, extra)
}

// recordWork leaves what a done job produced. A phase job leaves the document its role
// produces, for the phase gate to approve; a reply is its message in the conversation and
// leaves no document to approve, and a summary is the conversation's. Phase jobs and replies say
// whether the agent left a digest of what it did, and rebuild the task's working state.
func recordWork(ctx context.Context, q *db.Queries, j db.Job, fin proto.Finish) error {
	switch j.Kind {
	case KindSummary:
		return nil
	case KindPhase:
		content := artifactContent(j.Role, fin)
		typ := process.ArtifactTypeFor(j.Role)
		a, err := q.CreateArtifact(ctx, db.CreateArtifactParams{TaskID: j.TaskID, Phase: j.Phase, Type: typ, Content: &content})
		if err != nil {
			return err
		}
		if err := emit(ctx, q, "artifact.created", "task", j.TaskID, map[string]any{"phase": j.Phase, "type": typ, "version": a.Version, "job_id": fin.JobID}); err != nil {
			return err
		}
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

// trustDomain is whose login a job holds while it runs: its payer's, or "" for the runner's own.
func trustDomain(c CredentialChoice, j db.Job) string {
	if c.Runner() {
		return ""
	}
	return uuidString(j.PayerID)
}

// sharesTrust says whether a job of domain may run beside the runner's active jobs. The agents of
// one runner run as one user and can read each other's environment (/proc/<pid>/environ) and
// files, and no runner isolates its jobs from each other today: so a person's login runs only
// beside jobs on that same person's login, and the runner's own only beside jobs on the runner's.
func sharesTrust(active []string, domain string) bool {
	for _, d := range active {
		if d != domain {
			return false
		}
	}
	return true
}

// runnable is the runner's backends minus those whose command it does not have: a job on its
// payer's login needs no login of the runner's, but it does need the command.
func (s *session) runnable() []string {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	out := make([]string, 0, len(s.caps.Backends))
	for _, b := range s.caps.Backends {
		if s.auth[b] != "no_cli" {
			out = append(out, b)
		}
	}
	return out
}

// leasable is the runner's backends minus those it reports as logged out: a job on the runner's
// own login waits for a logged-in backend instead of running on another one.
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

// keeping are the backends of a runner that keep a limit, as it says; a runner that says nothing
// (one from before it could) keeps it with all of them.
func keeping(backends, says []string) []string {
	if says == nil {
		return backends
	}
	out := []string{}
	for _, b := range backends {
		if slices.Contains(says, b) {
			out = append(out, b)
		}
	}
	return out
}
