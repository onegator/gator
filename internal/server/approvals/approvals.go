// Package approvals is an agent waiting for a person mid-turn: a tool the project's approvals
// policy asks about, or a question. The runner asks (proto runtime_request), a card in the
// conversation shows it, a member of the project who may write there decides, and the decision
// goes back to the runner (runtime_decision). A request is decided once; a request nobody decides
// expires and is refused; a turn that ends cancels what it was waiting for.
package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

var (
	// ErrNotFound answers a request that does not exist, or that the caller may not see.
	ErrNotFound = errors.New("not found")
	// ErrForbidden answers a request the caller may see but not decide.
	ErrForbidden = errors.New("not allowed")
	// ErrConflict answers a request that is no longer waiting.
	ErrConflict = errors.New("conflict")
	// ErrInvalid answers a decision that cannot be taken as asked, and a request a runner had no
	// business sending (a refusal: runners.Refusal, so it is not sent again).
	ErrInvalid error = refusal("invalid")
)

type refusal string

func (r refusal) Error() string { return string(r) }

// Refusal implements runners.Refusal.
func (refusal) Refusal() {}

// Events of a request, on its job.
const (
	EventCreated   = "runtime_request.created"
	EventDecided   = "runtime_request.decided"
	EventExpired   = "runtime_request.expired"
	EventCancelled = "runtime_request.cancelled"
)

// Statuses of a request.
const (
	StatusPending           = "pending"
	StatusAllowed           = "allowed"
	StatusAllowedForSession = "allowed_for_session"
	StatusDenied            = "denied"
	StatusExpired           = "expired"
	StatusCancelled         = "cancelled"
)

// Runtime sends a decision to the runner that holds the job, and stops a job. runners.Manager is one.
type Runtime interface {
	SendDecision(ctx context.Context, job db.Job, d proto.RuntimeDecision) error
	StopJob(ctx context.Context, jobID pgtype.UUID, reason string) error
}

// conversationRights answers what a caller may do in a conversation. conversations.Access is one.
type conversationRights interface {
	RightsByID(ctx context.Context, id pgtype.UUID, p auth.Principal) (db.Conversation, conversations.Rights, error)
}

// roleResolver answers a caller's role in a project. auth.Authorizer is one.
type roleResolver interface {
	ProjectRole(ctx context.Context, p auth.Principal, project pgtype.UUID) (auth.Role, error)
}

// taskThreads answers a task's thread. conversations.Threads is one.
type taskThreads interface {
	ForTask(ctx context.Context, task db.Task) (db.Conversation, error)
}

// policies answers how long a project's requests wait. process.Service is one.
type policies interface {
	JobApprovals(ctx context.Context, projectID pgtype.UUID, agentPolicy []byte) (process.Approvals, error)
}

// Deps are what the package needs from the rest of the server.
type Deps struct {
	Rights   conversationRights
	Roles    roleResolver
	Threads  taskThreads
	Policies policies
	Log      *slog.Logger
	// Now is the clock of expiry; nil is time.Now.
	Now func() time.Time
}

// Service takes runners' requests and people's decisions.
type Service struct {
	pool    *pgxpool.Pool
	deps    Deps
	runtime Runtime
}

// New builds the service; UseRuntime gives it the runners before it decides anything.
func New(pool *pgxpool.Pool, deps Deps) *Service {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Service{pool: pool, deps: deps}
}

// UseRuntime is wiring: the runner manager that sends decisions takes requests from this service.
func (s *Service) UseRuntime(r Runtime) { s.runtime = r }

// Limits of what a request may say about itself.
const (
	maxTool     = 200
	maxRule     = 300
	maxToolUse  = 200
	maxFeedback = 4000
)

// Request keeps a runner's request, for the job it holds. A request sent again answers the
// decision it already has, for the runner to hear again (it may have missed it); nil while it
// waits. A job that only reads is never asked anything, and a request of it is refused.
func (s *Service) Request(ctx context.Context, job db.Job, r proto.RuntimeRequest) (*proto.RuntimeDecision, error) {
	var id pgtype.UUID
	switch {
	case id.Scan(r.ID) != nil || !id.Valid:
		return nil, fmt.Errorf("%w: request id %q", ErrInvalid, r.ID)
	case job.Tier == proto.TierChat:
		return nil, fmt.Errorf("%w: a job that reads is not asked about tools", ErrInvalid)
	case r.Kind != proto.RequestToolApproval && r.Kind != proto.RequestQuestion:
		return nil, fmt.Errorf("%w: kind %q", ErrInvalid, r.Kind)
	case len(r.Tool) > maxTool || len(r.Rule) > maxRule || len(r.ToolUseID) > maxToolUse || !utf8.ValidString(r.Tool+r.Rule):
		return nil, fmt.Errorf("%w: tool or rule", ErrInvalid)
	case len(r.Input) > proto.MaxRequestInput:
		return nil, fmt.Errorf("%w: an input over %d bytes", ErrInvalid, proto.MaxRequestInput)
	}
	if terminal(job.Status) {
		return nil, fmt.Errorf("%w: the job has ended", ErrInvalid)
	}
	input := []byte(r.Input)
	if !json.Valid(input) || len(input) == 0 {
		input = []byte("{}")
	}
	input = secrets.RedactValues(secrets.RedactJSON(input))
	wait, err := s.wait(ctx, job, r.TimeoutSeconds)
	if err != nil {
		return nil, err
	}
	var tool *string
	if r.Tool != "" {
		tool = &r.Tool
	}
	var decided *proto.RuntimeDecision
	err = store.InTx(ctx, s.pool, func(q *db.Queries) error {
		row, err := q.InsertRuntimeRequest(ctx, db.InsertRuntimeRequestParams{ID: id, JobID: job.ID, TurnID: job.TurnID, Kind: r.Kind,
			Tool: tool, Input: input, Rule: r.Rule, ToolUseID: r.ToolUseID, ExpiresAt: ts(s.deps.Now().Add(wait))})
		if errors.Is(err, pgx.ErrNoRows) {
			prior, err := q.GetRuntimeRequest(ctx, id)
			if err != nil {
				return err
			}
			if prior.JobID != job.ID {
				return fmt.Errorf("%w: request %s is another job's", ErrInvalid, r.ID)
			}
			if prior.Status != StatusPending {
				d := decisionOf(ctx, q, prior)
				decided = &d
			}
			return nil
		}
		if err != nil {
			return err
		}
		return emit(ctx, q, EventCreated, row)
	})
	return decided, err
}

// wait is how long a request of the job waits: the project's (with the agent's own) policy, and
// no longer than the runner itself waits.
func (s *Service) wait(ctx context.Context, job db.Job, runnerSeconds int) (time.Duration, error) {
	wait := process.DefaultApprovalTimeout
	if s.deps.Policies != nil {
		var agentPolicy []byte
		if job.AgentID.Valid {
			if a, err := db.New(s.pool).GetAgent(ctx, job.AgentID); err == nil {
				agentPolicy = a.Policy
			}
		}
		a, err := s.deps.Policies.JobApprovals(ctx, job.ProjectID, agentPolicy)
		if err != nil {
			return 0, err
		}
		wait = a.TimeoutOrDefault()
	}
	if r := time.Duration(runnerSeconds) * time.Second; r > 0 && r < wait {
		wait = r
	}
	return max(wait, time.Minute), nil
}

// Decisions a person takes.
const (
	DecideAllow           = proto.DecisionAllow
	DecideAllowForSession = proto.DecisionAllowForSession
	DecideDeny            = proto.DecisionDeny
	DecideStop            = proto.DecisionStop
)

// Decide is a person's decision on a request: allow (for a question, with the answer as feedback),
// allow for the rest of the session, deny (feedback is why, which the agent reads) or stop the
// turn. Only a member of the job's project who may write where the agent works decides, and only
// a request still waiting on a job still running: the check and the decision are one transaction
// on the request's row, so two people deciding at once get one decision and one conflict.
func (s *Service) Decide(ctx context.Context, p auth.Principal, id pgtype.UUID, decision, feedback string) (db.RuntimeRequest, error) {
	feedback = strings.TrimSpace(feedback)
	if utf8.RuneCountInString(feedback) > maxFeedback {
		return db.RuntimeRequest{}, fmt.Errorf("%w: feedback is at most %d characters", ErrInvalid, maxFeedback)
	}
	var out db.RuntimeRequest
	var job db.Job
	err := store.InTx(ctx, s.pool, func(q *db.Queries) error {
		req, err := q.GetRuntimeRequestForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if job, err = q.GetJob(ctx, req.JobID); err != nil {
			return err
		}
		if err := s.mayDecide(ctx, q, p, job); err != nil {
			return err
		}
		status, err := statusFor(req.Kind, decision, feedback)
		if err != nil {
			return err
		}
		switch {
		case req.Status != StatusPending:
			return fmt.Errorf("%w: this request is %s", ErrConflict, req.Status)
		case !req.ExpiresAt.Time.After(s.deps.Now()):
			return fmt.Errorf("%w: this request expired", ErrConflict)
		case terminal(job.Status):
			return fmt.Errorf("%w: the turn has ended", ErrConflict)
		}
		out, err = q.DecideRuntimeRequest(ctx, db.DecideRuntimeRequestParams{ID: id, Status: status, Feedback: optional(feedback),
			DecidedBy: p.UserID, Stopped: decision == DecideStop})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: this request was just decided", ErrConflict)
		}
		if err != nil {
			return err
		}
		return emit(ctx, q, EventDecided, out)
	})
	if err != nil {
		return db.RuntimeRequest{}, err
	}
	s.tell(ctx, job, out, p.Name)
	if decision == DecideStop {
		if err := s.runtime.StopJob(ctx, job.ID, "stopped from an approval by "+orText(p.Name, "a person")); err != nil {
			s.deps.Log.Warn("stopping a turn from its approval", "job", uuidString(job.ID), "err", err)
		}
	}
	return out, nil
}

// mayDecide says whether p decides the job's requests: a person, a member of the project, who may
// write where the agent works (its turn's conversation, else the task's thread). Somebody who may
// not even read there does not find the request.
func (s *Service) mayDecide(ctx context.Context, q *db.Queries, p auth.Principal, job db.Job) error {
	conv, err := s.home(ctx, q, job)
	if err != nil {
		return err
	}
	_, rights, err := s.deps.Rights.RightsByID(ctx, conv.ID, p)
	if errors.Is(err, conversations.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !rights.Read {
		return ErrNotFound
	}
	role, err := s.deps.Roles.ProjectRole(ctx, p, job.ProjectID)
	if err != nil {
		return err
	}
	if p.Kind != auth.KindUser || !p.UserID.Valid || role < auth.RoleMember || !rights.Write {
		return fmt.Errorf("%w: only a member of the project who may write here decides what an agent may do", ErrForbidden)
	}
	return nil
}

// home is where a job's agent works: its turn's conversation, else its task's thread.
func (s *Service) home(ctx context.Context, q *db.Queries, job db.Job) (db.Conversation, error) {
	if job.TurnID.Valid {
		turn, err := q.GetTurn(ctx, job.TurnID)
		if err == nil {
			return q.GetConversation(ctx, turn.ConversationID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return db.Conversation{}, err
		}
	}
	task, err := q.GetTask(ctx, job.TaskID)
	if err != nil {
		return db.Conversation{}, err
	}
	return s.deps.Threads.ForTask(ctx, task)
}

// statusFor is the status a decision leaves a request in.
func statusFor(kind, decision, feedback string) (string, error) {
	switch decision {
	case DecideAllow:
		if kind == proto.RequestQuestion && feedback == "" {
			return "", fmt.Errorf("%w: a question is answered with the answer as feedback", ErrInvalid)
		}
		return StatusAllowed, nil
	case DecideAllowForSession:
		if kind == proto.RequestQuestion {
			return "", fmt.Errorf("%w: a question is answered, not allowed for the session", ErrInvalid)
		}
		return StatusAllowedForSession, nil
	case DecideDeny, DecideStop:
		return StatusDenied, nil
	}
	return "", fmt.Errorf("%w: decision is allow, allow_for_session, deny or stop", ErrInvalid)
}

// DenyByMessage takes what a person writes in the conversation while an agent waits for a decision
// as a refusal of every request it waits on, with the message as the reason — the message steers
// the agent as well. Only a member who may decide refuses this way; for anyone else nothing
// changes. It answers how many requests it refused.
func (s *Service) DenyByMessage(ctx context.Context, job pgtype.UUID, author pgtype.UUID, body string) (int, error) {
	q := db.New(s.pool)
	u, err := q.GetUser(ctx, author)
	if err != nil {
		return 0, ignoreGone(err)
	}
	p := auth.Principal{Kind: auth.KindUser, UserID: u.ID, WorkspaceRole: u.WorkspaceRole, Name: u.Name}
	j, err := q.GetJob(ctx, job)
	if err != nil {
		return 0, ignoreGone(err)
	}
	if err := s.mayDecide(ctx, q, p, j); err != nil {
		if errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	feedback := strings.TrimSpace(body)
	if r := []rune(feedback); len(r) > maxFeedback {
		feedback = string(r[:maxFeedback])
	}
	var decided []db.RuntimeRequest
	err = store.InTx(ctx, s.pool, func(q *db.Queries) error {
		pending, err := q.ListPendingRuntimeRequestsOfJob(ctx, job)
		if err != nil {
			return err
		}
		for _, r := range pending {
			d, err := q.DecideRuntimeRequest(ctx, db.DecideRuntimeRequestParams{ID: r.ID, Status: StatusDenied, Feedback: optional(feedback),
				DecidedBy: u.ID})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if err := emit(ctx, q, EventDecided, d); err != nil {
				return err
			}
			decided = append(decided, d)
		}
		return nil
	})
	for _, d := range decided {
		s.tell(ctx, j, d, u.Name)
	}
	return len(decided), err
}

// Expire refuses the requests nobody decided in time, and tells their runners.
func (s *Service) Expire(ctx context.Context) (int, error) {
	var expired []db.RuntimeRequest
	err := store.InTx(ctx, s.pool, func(q *db.Queries) error {
		var err error
		if expired, err = q.ExpireRuntimeRequests(ctx, ts(s.deps.Now())); err != nil {
			return err
		}
		for _, r := range expired {
			if err := emit(ctx, q, EventExpired, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	q := db.New(s.pool)
	for _, r := range expired {
		if job, err := q.GetJob(ctx, r.JobID); err == nil {
			s.tell(ctx, job, r, "")
		}
	}
	return len(expired), nil
}

// CancelForJob ends what a job that ended was waiting for.
func (s *Service) CancelForJob(ctx context.Context, job pgtype.UUID) error {
	return store.InTx(ctx, s.pool, func(q *db.Queries) error {
		cancelled, err := q.CancelJobRuntimeRequests(ctx, job)
		if err != nil {
			return err
		}
		for _, r := range cancelled {
			if err := emit(ctx, q, EventCancelled, r); err != nil {
				return err
			}
		}
		return nil
	})
}

// Run expires requests until ctx ends.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Expire(ctx); err != nil && ctx.Err() == nil {
				s.deps.Log.Warn("expiring runtime requests", "err", err)
			}
		}
	}
}

// tell sends a decided request's decision to the runner that holds its job. A runner that is away
// sends its request again when it is back, and hears the decision then.
func (s *Service) tell(ctx context.Context, job db.Job, r db.RuntimeRequest, by string) {
	if s.runtime == nil {
		return
	}
	d := decisionOf(ctx, db.New(s.pool), r)
	if by != "" {
		d.By = by
	}
	if err := s.runtime.SendDecision(ctx, job, d); err != nil {
		s.deps.Log.Info("a decision waits for its runner", "job", uuidString(job.ID), "request", uuidString(r.ID), "err", err)
	}
}

// decisionOf is a decided request as the runner hears it.
func decisionOf(ctx context.Context, q *db.Queries, r db.RuntimeRequest) proto.RuntimeDecision {
	d := proto.RuntimeDecision{ID: uuidString(r.ID), JobID: uuidString(r.JobID), Feedback: deref(r.Feedback)}
	switch {
	case r.Status == StatusAllowed:
		d.Decision = proto.DecisionAllow
	case r.Status == StatusAllowedForSession:
		d.Decision = proto.DecisionAllowForSession
	case r.Status == StatusDenied && r.Stopped:
		d.Decision = proto.DecisionStop
	case r.Status == StatusDenied:
		d.Decision = proto.DecisionDeny
	case r.Status == StatusExpired:
		d.Decision = proto.DecisionExpired
	default:
		d.Decision = proto.DecisionCancelled
	}
	if r.DecidedBy.Valid {
		if u, err := q.GetUser(ctx, r.DecidedBy); err == nil {
			d.By = u.Name
		}
	}
	return d
}

func emit(ctx context.Context, q *db.Queries, typ string, r db.RuntimeRequest) error {
	payload, _ := json.Marshal(map[string]any{"request_id": uuidString(r.ID), "kind": r.Kind, "tool": deref(r.Tool),
		"status": r.Status, "turn_id": uuidString(r.TurnID)})
	_, err := q.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: "job", AggregateID: r.JobID, Payload: payload})
	return err
}

func terminal(status string) bool {
	return status == proto.StatusDone || status == proto.StatusFailed || status == proto.StatusStopped
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func orText(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func uuidString(u pgtype.UUID) string { return conversations.UUIDString(u) }

func ignoreGone(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// Get answers a request for someone who may read where its agent works.
func (s *Service) Get(ctx context.Context, p auth.Principal, id pgtype.UUID) (db.RuntimeRequest, error) {
	q := db.New(s.pool)
	r, err := q.GetRuntimeRequest(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.RuntimeRequest{}, ErrNotFound
	}
	if err != nil {
		return db.RuntimeRequest{}, err
	}
	job, err := q.GetJob(ctx, r.JobID)
	if err != nil {
		return db.RuntimeRequest{}, err
	}
	conv, err := s.home(ctx, q, job)
	if err != nil {
		return db.RuntimeRequest{}, err
	}
	_, rights, err := s.deps.Rights.RightsByID(ctx, conv.ID, p)
	if errors.Is(err, conversations.ErrNotFound) || (err == nil && !rights.Read) {
		return db.RuntimeRequest{}, ErrNotFound
	}
	return r, err
}

// ForJob answers a job's requests; the caller has made sure the job is theirs to read.
func (s *Service) ForJob(ctx context.Context, job pgtype.UUID) ([]db.RuntimeRequest, error) {
	return db.New(s.pool).ListJobRuntimeRequests(ctx, job)
}

// Waiting answers the requests waiting for the caller to decide, newest first ("Needs you"):
// those of projects it may decide in, optionally of one project.
func (s *Service) Waiting(ctx context.Context, p auth.Principal, project pgtype.UUID) ([]db.RuntimeRequest, error) {
	q := db.New(s.pool)
	projects := []pgtype.UUID{}
	if project.Valid {
		projects = append(projects, project)
	}
	rows, err := q.ListPendingRuntimeRequests(ctx, projects)
	if err != nil {
		return nil, err
	}
	out := []db.RuntimeRequest{}
	for _, r := range rows {
		job, err := q.GetJob(ctx, r.JobID)
		if err != nil {
			return nil, err
		}
		err = s.mayDecide(ctx, q, p, job)
		switch {
		case err == nil:
			out = append(out, r)
		case errors.Is(err, ErrForbidden), errors.Is(err, ErrNotFound):
		default:
			return nil, err
		}
	}
	return out, nil
}
