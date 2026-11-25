// Package api implements the HTTP API declared in docs/openapi.yaml.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
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
	"github.com/onegator/gator/internal/server/a2a"
	"github.com/onegator/gator/internal/server/agents"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/automations"
	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/bridges"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/invites"
	"github.com/onegator/gator/internal/server/limits"
	"github.com/onegator/gator/internal/server/marketplace"
	"github.com/onegator/gator/internal/server/mcpremote"
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
	Pool    *pgxpool.Pool
	Process *process.Service
	Runners *runners.Manager
	Plugins *plugins.Host // nil: plugin endpoints answer 503
	Quality *quality.Service
	// Automations is the project's catalogue of automations; nil: those endpoints answer 503.
	Automations *automations.Service
	Hub         *events.Hub
	Tokens      auth.Tokens
	Authz       auth.Authorizer
	OIDC        *auth.OIDC // nil when not configured
	DevAuth     bool       // accept X-Gator-User; local development only
	Features    []string
	Log         *slog.Logger
	// RateLimit applies per token (or IP when anonymous). Zero disables.
	RateLimitPerSecond float64
	RateLimitBurst     int
	// TrustedProxies may name the client in X-Forwarded-For; everyone else is who they
	// connect from.
	TrustedProxies limits.Proxies
	// PublicURL is where people reach this server, for the invitation links they pass on.
	// Empty: the address the inviter used.
	PublicURL string
	// Mailer delivers invitations; nil leaves that to the inviter, who gets the link.
	Mailer invites.Mailer
	// Conversations are channels, DMs and threads; nil: those endpoints answer 503.
	Conversations *conversations.Module
	// Agents are the project's members that answer in conversations; nil: their endpoints
	// answer 503.
	Agents *agents.Module
	// Brain is what the team and its agents know; nil: its endpoints answer 503.
	Brain *brain.Module
	// MCP serves the gator-brain MCP server at /mcp, for jobs' and people's tokens; nil: no /mcp.
	MCP http.Handler
	// Marketplace is where agents, skills, integrations and processes come from; nil: its
	// endpoints answer 503.
	Marketplace *marketplace.Module
	// Remote is the remote MCP servers agents reach through Gator's proxy (/mcp/proxy/<name>);
	// nil: no proxy, and their endpoints answer 503.
	Remote *mcpremote.Module
	// Bridges are the messaging plugins' side of the core (account links); nil: their endpoints
	// answer 503.
	Bridges *bridges.Service
	// A2A is agents outside Gator, both ways; nil: their endpoints answer 503.
	A2A *a2a.Module
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
	r.Get("/invite/{token}", invitePageHandler)
	if s.MCP != nil {
		// Streamable HTTP: POST carries the calls; the server is stateless, so GET and DELETE
		// answer 405 from the SDK itself.
		r.With(auth.RequireAuth).Handle(auth.MCPPath, s.MCP)
	}
	if s.Remote != nil {
		// A job reaches the remote servers its agent was given here, with its own token; the
		// servers' credentials stay on this side.
		r.With(auth.RequireAuth).Handle(mcpremote.PathPrefix+"*", s.Remote.Proxy)
	}
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
	if s.Conversations != nil {
		// A project starts with #general and its creator in it. Should this fail, the project
		// still exists, and the channel comes with the project's first task.
		if _, err := s.Conversations.Directory.EnsureDefaultChannels(r.Context(), pr.ID, p.UserID); err != nil {
			s.Log.Warn("default channels", "project", pr.ID, "err", err)
		}
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

// AllowAgents is a person allowing agents to work on a task without asking for permission.
// Until somebody does, no job starts on it: an agent runs as the runner's user, with that
// machine's files, network and credentials, and nothing stops it to ask first.
func (s *Server) AllowAgents(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	p, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember)
	if !ok {
		return
	}
	if p.Kind != auth.KindUser {
		writeError(w, http.StatusForbidden, "only a person can allow agents to work without asking", "forbidden")
		return
	}
	if _, err := s.Process.AllowAgents(r.Context(), fromUUID(taskId), ActorFromContext(r.Context())); err != nil {
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

// wsOrigins are the browser origins besides the request's own host that may open the
// WebSocket: the host of GATOR_PUBLIC_URL, when the server sits behind a proxy that changes
// the Host it sees.
func (s *Server) wsOrigins() []string {
	if u, err := url.Parse(s.PublicURL); err == nil && u.Host != "" {
		return []string{u.Host}
	}
	return nil
}

type wsSubscribe struct {
	Subscribe []string `json:"subscribe"`
}

// wsClientMessage is what a client may send after subscribing: that it is typing.
type wsClientMessage struct {
	Typing string `json:"typing"`
}

// websocket streams hub events. The client sends {"subscribe":["inbox","task:<id>"]} first
// and may send it again to change topics. "project:<id>" carries everything that happens in
// one project and "conversation:<id>" everything said in one conversation, so each is offered
// only to those who may read it; "me" is what concerns the caller. Whatever the topic, an event
// that belongs to a project reaches the caller only if they may read that project, and one about
// a conversation only if they may read that conversation (see eventFilter).
func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	// Gator.app, the CLI and runners send no Origin and are let in. A browser always sends one,
	// and its session cookie rides along, so a page on another site could listen in on the
	// caller's events: only this server's own address, or GATOR_PUBLIC_URL's, may open one.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.wsOrigins()})
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
	topics, refused := s.hubTopics(r, sub.Subscribe)
	if refused != "" {
		_ = c.Close(websocket.StatusPolicyViolation, "not allowed to follow "+refused)
		return
	}
	ch, stop := s.Hub.Subscribe(topics...)
	defer stop()
	p, _ := auth.FromContext(ctx)
	visible := newEventFilter(s, p)
	typing := newTypingRelay(s, p)

	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m wsClientMessage
			if json.Unmarshal(b, &m) == nil && m.Typing != "" {
				typing.say(ctx, m.Typing)
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			if !visible.allow(ctx, e) {
				continue
			}
			b, _ := json.Marshal(e)
			if err := c.Write(ctx, websocket.MessageText, b); err != nil {
				return
			}
		}
	}
}

// hubTopics turns the topics a client asked for into the hub's, or names the first one the
// caller may not follow. "me" is the caller's own "user:<id>", which nobody asks for by name.
func (s *Server) hubTopics(r *http.Request, topics []string) ([]string, string) {
	p, _ := auth.FromContext(r.Context())
	out := make([]string, 0, len(topics))
	for _, t := range topics {
		switch {
		case t == "me":
			if p.Kind != auth.KindUser || !p.UserID.Valid {
				return nil, t
			}
			out = append(out, "user:"+uuidString(p.UserID))
		case strings.HasPrefix(t, "user:"):
			return nil, t
		case strings.HasPrefix(t, "project:"):
			var project pgtype.UUID
			if err := project.Scan(strings.TrimPrefix(t, "project:")); err != nil {
				return nil, t
			}
			if err := s.Authz.Require(r.Context(), p, project, auth.RoleViewer); err != nil {
				return nil, t
			}
			out = append(out, t)
		case strings.HasPrefix(t, "conversation:"):
			var conv pgtype.UUID
			if s.Conversations == nil || conv.Scan(strings.TrimPrefix(t, "conversation:")) != nil {
				return nil, t
			}
			if _, rights, err := s.Conversations.Access.RightsByID(r.Context(), conv, p); err != nil || !rights.Read {
				return nil, t
			}
			out = append(out, t)
		default:
			out = append(out, t)
		}
	}
	return out, ""
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
	switch {
	case errors.Is(err, runners.ErrAgentsNotAllowed):
		writeError(w, http.StatusConflict, "nobody has allowed agents on this task yet: agents work without asking for permission, "+
			"so a person allows it once per task (POST /tasks/{id}/allow-agents, or allowAgents: true on the job)", "agents_not_allowed")
		return
	case errors.Is(err, process.ErrConsentNeedsPerson):
		writeError(w, http.StatusForbidden, err.Error(), "forbidden")
		return
	}
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
		errors.Is(err, process.ErrTerminalPhase), errors.Is(err, process.ErrKindLocked):
		writeError(w, http.StatusConflict, err.Error(), "conflict")
	case errors.Is(err, process.ErrInvalidUsage), errors.Is(err, process.ErrInvalidEdit),
		errors.Is(err, process.ErrHiddenKind), errors.Is(err, process.ErrNotAnAsk):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
	default:
		s.Log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error", "internal")
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error(), "invalid")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error(), "invalid")
		return false
	}
	if missing := missingRequired(raw, v); missing != "" {
		writeError(w, http.StatusBadRequest, "invalid body: "+missing+" is required", "invalid")
		return false
	}
	return true
}

// missingRequired names the first field the spec requires that the body left out or sent null.
// The generated types say which: a required field is a value with a json tag without omitempty
// (an optional one is a pointer with omitempty). Without this, {} would set a required boolean to
// false and a required object to null — "config is required" stored as no config at all.
func missingRequired(raw []byte, v any) string {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return ""
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	// encoding/json matches keys without regard to case ({"Enabled": true} sets enabled), so this
	// does too: a key the decoder took is a key that was sent.
	sent := make(map[string]json.RawMessage, len(body))
	for k, v := range body {
		sent[strings.ToLower(k)] = v
	}
	for i := range t.NumField() {
		f := t.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || strings.Contains(opts, "omitempty") {
			continue
		}
		if val, ok := sent[strings.ToLower(name)]; !ok || string(val) == "null" {
			return name
		}
	}
	return ""
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
