// Package api implements the HTTP API declared in docs/openapi.yaml.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/version"
)

// Server implements gen.ServerInterface.
type Server struct {
	Pool     *pgxpool.Pool
	Process  *process.Service
	Hub      *events.Hub
	Tokens   auth.Tokens
	Authz    auth.Authorizer
	OIDC     *auth.OIDC // nil when not configured
	DevAuth  bool       // accept X-Gator-User; local development only
	Features []string
	Log      *slog.Logger
}

var _ gen.ServerInterface = (*Server)(nil)

// Router mounts the API under /api/v1 plus the WebSocket endpoint.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)
	r.Use(auth.Authenticate(s.Tokens, s.DevAuth, s.Log))
	r.Use(auth.AuditWith(db.New(s.Pool), s.Log))
	r.Route("/auth", func(r chi.Router) {
		if s.OIDC != nil {
			r.Get("/login", s.OIDC.Login)
			r.Get("/callback", s.OIDC.Callback)
		} else {
			r.Get("/login", func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusNotImplemented, "OIDC is not configured (GATOR_OIDC_*)", "oidc_disabled")
			})
		}
		r.Post("/logout", auth.Logout(s.Tokens))
	})
	r.Route("/api/v1", func(r chi.Router) {
		r.With(auth.RequireAuth).Get("/ws", s.websocket)
		gen.HandlerFromMux(s, r)
	})
	return r
}

// --- meta ---

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
	out := make([]gen.Project, 0, len(rows))
	for _, pr := range rows {
		if visible == nil || visible[pr.ID.Bytes] {
			out = append(out, toProject(pr))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) CreateProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "creating projects requires workspace admin", "forbidden")
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
	tags := []string{}
	if in.Tags != nil {
		tags = *in.Tags
	}
	pr, err := db.New(s.Pool).CreateProject(r.Context(), db.CreateProjectParams{Slug: in.Slug, Name: in.Name, Tags: tags, ProcessConfig: []byte("{}")})
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := db.New(s.Pool).UpsertMembership(r.Context(), db.UpsertMembershipParams{ProjectID: pr.ID, UserID: p.UserID, Role: "admin"}); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toProject(pr))
}

func (s *Server) GetProject(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleViewer); !ok {
		return
	}
	p, err := db.New(s.Pool).GetProject(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toProject(p))
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
	out := make([]gen.Task, 0, len(rows))
	for _, t := range rows {
		out = append(out, toTask(t))
	}
	writeJSON(w, http.StatusOK, out)
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
	p := process.CreateParams{ProjectID: fromUUID(projectId), Kind: string(in.Kind), Title: in.Title}
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
	rows, err := s.Process.Inbox(r.Context(), pid)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.Decision, 0, len(rows))
	for _, d := range rows {
		if visible != nil && !visible[d.Task.ProjectID.Bytes] {
			continue
		}
		out = append(out, gen.Decision{Task: toTask(d.Task), Reason: gen.DecisionReason(d.Reason), WaitingSince: d.WaitingSince})
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
	out := make([]gen.Transition, 0, len(rows))
	for _, tr := range rows {
		out = append(out, toTransition(tr))
	}
	writeJSON(w, http.StatusOK, out)
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
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not found", "not_found")
	case errors.Is(err, process.ErrGateNotSatisfied), errors.Is(err, process.ErrTaskBlocked),
		errors.Is(err, process.ErrTaskClosed), errors.Is(err, process.ErrNotBackward),
		errors.Is(err, process.ErrRollbackCeiling), errors.Is(err, process.ErrUnknownPhase),
		errors.Is(err, process.ErrTerminalPhase):
		writeError(w, http.StatusConflict, err.Error(), "conflict")
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
