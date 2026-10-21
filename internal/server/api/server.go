// Package api implements the HTTP API declared in docs/openapi.yaml.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/limits"
	"github.com/onegator/gator/internal/server/plugins"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/quality"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/server/telemetry"
	"github.com/onegator/gator/internal/version"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
)

// Server implements gen.ServerInterface.
type Server struct {
	Pool     *pgxpool.Pool
	Process  *process.Service
	Runners  *runners.Manager
	Plugins  *plugins.Host // nil: plugin endpoints answer 503
	Quality  *quality.Service
	Hub      *events.Hub
	Tokens   auth.Tokens
	Authz    auth.Authorizer
	OIDC     *auth.OIDC // nil when not configured
	DevAuth  bool       // accept X-Gator-User; local development only
	Features []string
	Log      *slog.Logger
	// RateLimit applies per token (or IP when anonymous). Zero disables.
	RateLimitPerSecond float64
	RateLimitBurst     int
	// TrustedProxies may name the client in X-Forwarded-For; everyone else is who they
	// connect from.
	TrustedProxies limits.Proxies
}

var _ gen.ServerInterface = (*Server)(nil)

// Router mounts the API under /api/v1 plus the WebSocket endpoint.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, s.TrustedProxies.RealIP, middleware.Recoverer)
	r.Use(func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "http", otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
				return r.Method + " " + rc.RoutePattern()
			}
			return r.Method + " " + r.URL.Path
		}))
	})
	if s.RateLimitPerSecond > 0 {
		lim := limits.NewLimiter(s.RateLimitPerSecond, s.RateLimitBurst)
		r.Use(limits.Middleware(lim, limits.ByBearerOrIP, func(r *http.Request) {
			if m, err := telemetry.Instruments(); err == nil {
				m.HTTPRateLimited.Add(r.Context(), 1, metric.WithAttributes(telemetry.Attr("scope", "api")))
			}
		}))
	}
	r.Use(auth.Authenticate(s.Tokens, s.DevAuth, s.Log))
	r.Use(auth.AuditWith(db.New(s.Pool), s.Log))
	r.Route("/auth", func(r chi.Router) {
		r.Get("/config", s.authConfig)
		if s.OIDC != nil {
			r.Get("/login", s.OIDC.Login)
			r.Get("/callback", s.OIDC.Callback)
			r.Post("/token", s.OIDC.Token)
		} else {
			r.Get("/login", func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusNotImplemented, "OIDC is not configured (GATOR_OIDC_*)", "oidc_disabled")
			})
		}
		r.Post("/logout", auth.Logout(s.Tokens))
	})
	r.Route("/api/v1", func(r chi.Router) {
		r.With(auth.RequireAuth).Get("/ws", s.websocket)
		if s.Runners != nil {
			r.With(auth.RequireAuth).Get("/runner", s.Runners.ServeHTTP)
		}
		gen.HandlerFromMux(s, r)
	})
	return r
}

// --- meta ---

// authConfig tells a client how it can sign in before it has any token.
func (s *Server) authConfig(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]any{"oidc": s.OIDC != nil}
	if s.OIDC != nil {
		resp["provider"] = s.OIDC.ProviderName()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, gen.Status{Status: "ok"})
}

func (s *Server) Readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{"database": "ok"}
	status := http.StatusOK
	if err := s.Pool.Ping(ctx); err != nil {
		checks["database"] = err.Error()
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, gen.Readiness{Checks: checks})
}

func (s *Server) Version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, gen.Version{Version: version.Version, Commit: version.Commit, Date: version.Date})
}

func (s *Server) Capabilities(w http.ResponseWriter, _ *http.Request) {
	f := s.Features
	if f == nil {
		f = []string{}
	}
	writeJSON(w, http.StatusOK, gen.Capabilities{Features: f, ProtocolVersion: proto.Version})
}

// --- projects ---

func (s *Server) ListProjects(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	visible, err := s.visibleProjects(r, p)
	if err != nil {
		s.fail(w, err)
		return
	}
	rows, err := db.New(s.Pool).ListProjects(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	roles, err := s.userRoles(r, p)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.Project, 0, len(rows))
	for _, pr := range rows {
		if visible == nil || visible[pr.ID.Bytes] {
			out = append(out, withRole(toProject(pr), roles(pr.ID)))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// userRoles answers the caller's role per project from one read of their memberships.
// Runners and plugins have no role worth showing; they get none.
func (s *Server) userRoles(r *http.Request, p auth.Principal) (func(pgtype.UUID) string, error) {
	switch {
	case p.Kind != auth.KindUser:
		return func(pgtype.UUID) string { return "" }, nil
	case p.IsWorkspaceAdmin():
		return func(pgtype.UUID) string { return "admin" }, nil
	}
	ms, err := db.New(s.Pool).ListMembershipsForUser(r.Context(), p.UserID)
	if err != nil {
		return nil, err
	}
	byProject := make(map[[16]byte]string, len(ms))
	for _, m := range ms {
		byProject[m.ProjectID.Bytes] = m.Role
	}
	return func(id pgtype.UUID) string { return byProject[id.Bytes] }, nil
}

// roleIn is userRoles for a single project.
func (s *Server) roleIn(r *http.Request, p auth.Principal, project pgtype.UUID) string {
	roles, err := s.userRoles(r, p)
	if err != nil {
		return ""
	}
	return roles(project)
}

func withRole(p gen.Project, role string) gen.Project {
	if role != "" {
		r := gen.ProjectRole(role)
		p.Role = &r
	}
	return p
}

func (s *Server) CreateProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	// Anyone signed in may start a project and runs it; a runner or a plugin may not.
	if p.Kind != auth.KindUser {
		writeError(w, http.StatusForbidden, "only a person can create a project", "forbidden")
		return
	}
	var in gen.NewProject
	if !decode(w, r, &in) {
		return
	}
	if in.Slug == "" || in.Name == "" {
		writeError(w, http.StatusBadRequest, "slug and name are required", "invalid")
		return
	}
	var repo *repoInput
	if in.RepoUrl != nil && strings.TrimSpace(*in.RepoUrl) != "" {
		ri, err := parseRepo(in.Slug, *in.RepoUrl, in.DefaultBranch)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid")
			return
		}
		repo = &ri
	}
	tags := []string{}
	if in.Tags != nil {
		tags = *in.Tags
	}
	pr, err := db.New(s.Pool).CreateProject(r.Context(), db.CreateProjectParams{Slug: in.Slug, Name: in.Name, Tags: tags, ProcessConfig: []byte("{}")})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, fmt.Sprintf("a project called %q already exists; pick another short name", in.Slug), "conflict")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := db.New(s.Pool).UpsertMembership(r.Context(), db.UpsertMembershipParams{ProjectID: pr.ID, UserID: p.UserID, Role: "admin"}); err != nil {
		s.fail(w, err)
		return
	}
	if in.Preset != nil && *in.Preset != "" {
		// The project exists either way: a preset that cannot be applied leaves an empty
		// project rather than swallowing one that was already created and named.
		if err := s.applyPreset(r.Context(), *in.Preset, pr, p.UserID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid")
			return
		}
		if updated, err := db.New(s.Pool).GetProject(r.Context(), pr.ID); err == nil {
			pr = updated
		}
	}
	// Given after the preset, so the repository named here wins over the preset's.
	if repo != nil {
		if _, err := s.putRepo(r.Context(), pr.ID, *repo, true); err != nil {
			s.fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, withRole(toProject(pr), "admin"))
}

func (s *Server) GetProject(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	caller, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer)
	if !ok {
		return
	}
	p, err := db.New(s.Pool).GetProject(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, withRole(toProject(p), s.roleIn(r, caller, p.ID)))
}

// --- tasks ---

func (s *Server) ListTasks(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	rows, err := s.Process.Tasks(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, toTask))
}

func (s *Server) CreateTask(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleMember); !ok {
		return
	}
	var in gen.NewTask
	if !decode(w, r, &in) {
		return
	}
	if in.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required", "invalid")
		return
	}
	p := process.CreateParams{ProjectID: fromUUID(projectId), Kind: string(in.Kind), Title: in.Title, Description: deref(in.Description)}
	if in.Urgency != nil {
		p.Urgency = int16(*in.Urgency)
	}
	t, err := s.Process.Create(r.Context(), p, ActorFromContext(r.Context()))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toTask(t))
}

func (s *Server) ListInbox(w http.ResponseWriter, r *http.Request, params gen.ListInboxParams) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	var pid pgtype.UUID
	if params.ProjectId != nil {
		pid = fromUUID(*params.ProjectId)
		if _, ok := s.requireProject(w, r, pid, auth.RoleViewer); !ok {
			return
		}
	}
	visible, err := s.visibleProjects(r, p)
	if err != nil {
		s.fail(w, err)
		return
	}
	var owner pgtype.UUID
	if params.Mine != nil && *params.Mine {
		owner = p.UserID
	}
	rows, err := s.Process.Inbox(r.Context(), process.InboxFilter{ProjectID: pid, OwnerID: owner})
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.Decision, 0, len(rows))
	for _, d := range rows {
		if visible != nil && !visible[d.Task.ProjectID.Bytes] {
			continue
		}
		rollbacks := d.Rollbacks
		out = append(out, gen.Decision{Task: toTask(d.Task), Reason: gen.DecisionReason(d.Reason), WaitingSince: d.WaitingSince, Rollbacks: &rollbacks})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) GetTask(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleViewer); !ok {
		return
	}
	d, err := s.Process.Detail(r.Context(), fromUUID(taskId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toDetail(d))
}

func (s *Server) ApproveTask(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	p, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember)
	if !ok {
		return
	}
	if p.Kind != auth.KindUser {
		writeError(w, http.StatusForbidden, "approval requires a user identity", "forbidden")
		return
	}
	actor := ActorFromContext(r.Context())
	if err := s.Process.Approve(r.Context(), fromUUID(taskId), actor.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.respondTask(w, r, taskId)
}

// AdmitTask lets work raised outside the workspace through. Until a person does this, no agent
// starts on it by itself: the words in it were written by somebody this workspace does not
// vouch for, and an agent runs with the operator's credentials.
func (s *Server) AdmitTask(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	p, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember)
	if !ok {
		return
	}
	if p.Kind != auth.KindUser {
		writeError(w, http.StatusForbidden, "admitting outside work requires a user identity", "forbidden")
		return
	}
	if _, err := s.Process.Admit(r.Context(), fromUUID(taskId), ActorFromContext(r.Context())); err != nil {
		if errors.Is(err, process.ErrNotExternal) {
			writeError(w, http.StatusBadRequest, "this task was raised inside the workspace; there is nothing to admit", "invalid")
			return
		}
		s.fail(w, err)
		return
	}
	s.respondTask(w, r, taskId)
}

func (s *Server) AdvanceTask(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.Reason
	_ = json.NewDecoder(r.Body).Decode(&in)
	t, err := s.Process.Advance(r.Context(), fromUUID(taskId), ActorFromContext(r.Context()), deref(in.Reason))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTask(t))
}

func (s *Server) RollbackTask(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.Rollback
	if !decode(w, r, &in) {
		return
	}
	t, err := s.Process.Rollback(r.Context(), fromUUID(taskId), in.To, ActorFromContext(r.Context()), in.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTask(t))
}

func (s *Server) HandoffTask(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.Handoff
	if !decode(w, r, &in) {
		return
	}
	err := s.Process.Handoff(r.Context(), fromUUID(taskId), process.ActorKind(in.ToKind), fromUUID(in.ToId), ActorFromContext(r.Context()), deref(in.Reason))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respondTask(w, r, taskId)
}

func (s *Server) SetTaskCheck(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.Check
	if !decode(w, r, &in) {
		return
	}
	c := process.Check{Name: in.Name, Source: in.Source, Status: string(in.Status), Detail: deref(in.Detail)}
	if err := s.Process.SetCheck(r.Context(), fromUUID(taskId), c); err != nil {
		s.fail(w, err)
		return
	}
	s.respondTask(w, r, taskId)
}

func (s *Server) ListTaskTransitions(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleViewer); !ok {
		return
	}
	rows, err := s.Process.Transitions(r.Context(), fromUUID(taskId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, toTransition))
}

// --- websocket ---

type wsSubscribe struct {
	Subscribe []string `json:"subscribe"`
}

// websocket streams hub events. The client sends {"subscribe":["inbox","task:<id>"]} first
// and may send it again to change topics.
func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()

	_, first, err := c.Read(ctx)
	if err != nil {
		return
	}
	var sub wsSubscribe
	if err := json.Unmarshal(first, &sub); err != nil || len(sub.Subscribe) == 0 {
		_ = c.Close(websocket.StatusPolicyViolation, "first message must be {\"subscribe\":[...]}")
		return
	}
	ch, stop := s.Hub.Subscribe(sub.Subscribe...)
	defer stop()

	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			b, _ := json.Marshal(e)
			if err := c.Write(ctx, websocket.MessageText, b); err != nil {
				return
			}
		}
	}
}

// --- helpers ---

func (s *Server) respondTask(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	t, err := db.New(s.Pool).GetTask(r.Context(), fromUUID(taskId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTask(t))
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if code, ok := runnerErrStatus(err); ok {
		writeError(w, code, err.Error(), http.StatusText(code))
		return
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not found", "not_found")
	case errors.Is(err, process.ErrGateNotSatisfied), errors.Is(err, process.ErrTaskBlocked),
		errors.Is(err, process.ErrTaskClosed), errors.Is(err, process.ErrNotBackward),
		errors.Is(err, process.ErrRollbackCeiling), errors.Is(err, process.ErrUnknownPhase),
		errors.Is(err, process.ErrTerminalPhase):
		writeError(w, http.StatusConflict, err.Error(), "conflict")
	case errors.Is(err, process.ErrInvalidUsage):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
	default:
		s.Log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error", "internal")
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error(), "invalid")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg, code string) {
	writeJSON(w, status, gen.Error{Error: msg, Code: &code})
}

// mapAll converts every row for the response. It never returns nil: an empty list is
// [] in JSON, not null.
func mapAll[T, U any](rows []T, to func(T) U) []U {
	out := make([]U, 0, len(rows))
	for _, r := range rows {
		out = append(out, to(r))
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Server) ListTaskArtifacts(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleViewer); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListArtifacts(r.Context(), fromUUID(taskId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, toArtifact))
}
