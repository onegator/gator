// Package mcp is the gator-brain MCP server at /mcp (Streamable HTTP): the Brain and the agent
// surface, as tools an agent's MCP client finds by itself. A job's own token reaches what its kind
// may call (jobscope, the same sets the REST agent surface asks); a person's token reaches what
// that person reads in the app, so `claude mcp add --transport http gator …/mcp` puts their Brain
// in their own Claude Code, Cursor or Codex. Every call a job makes is written into agent_calls.
package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/jobscope"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/version"
)

// Name is the server's name in a client's configuration and in its tools (mcp__gator__…).
const Name = "gator"

// Caller is who calls: a person, or a job's own identity.
type Caller struct {
	Principal auth.Principal
	// Reader is the caller as the Brain's rules see them.
	Reader brain.Reader
	// Job is the job behind a job token; zero for a person.
	Job db.Job
	// scope is what the job's kind may call.
	scope jobscope.Set
}

// IsJob says whether a job's identity calls.
func (c Caller) IsJob() bool { return c.Job.ID.Valid }

// store is what the server reads and writes itself. *db.Queries is one.
type store interface {
	GetJob(ctx context.Context, id pgtype.UUID) (db.Job, error)
	RecordAgentCall(ctx context.Context, arg db.RecordAgentCallParams) error
}

// readers works out who reads the Brain. brain.Readers is one.
type readers interface {
	ForPrincipal(ctx context.Context, p auth.Principal) (brain.Reader, error)
	ForJob(ctx context.Context, job db.Job) (brain.Reader, error)
}

// Tool is one tool of the server. A new tool is one more value in the list the server is built
// with; nothing else changes.
type Tool interface {
	Name() string
	// Allowed says whether c may see and call it.
	Allowed(c Caller) bool
	// Add puts it on s, for c.
	Add(s *sdk.Server, c Caller, rec Recorder)
}

// Resource is one resource (or template of them) of the server.
type Resource interface {
	Allowed(c Caller) bool
	Add(s *sdk.Server, c Caller)
}

// Recorder writes down a call a job made.
type Recorder func(ctx context.Context, c Caller, tool string, fields map[string]any)

// Server serves /mcp.
type Server struct {
	store     store
	readers   readers
	tools     []Tool
	resources []Resource
	log       *slog.Logger
	handler   http.Handler
}

// New builds the server with its tools and resources.
func New(st store, rs readers, tools []Tool, resources []Resource, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{store: st, readers: rs, tools: tools, resources: resources, log: log}
	// Stateless: every request is a session of its own, made for the caller it authenticated, so
	// no session outlives a revoked token and nothing is kept between calls.
	s.handler = sdk.NewStreamableHTTPHandler(s.serverFor, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: log})
	return s
}

type callerKey struct{}

// ServeHTTP authenticates the caller, then serves MCP for them.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		refuse(w, http.StatusUnauthorized, "a person's or a job's token is required")
		return
	}
	c, status, why := s.caller(r.Context(), p)
	if status != 0 {
		refuse(w, status, why)
		return
	}
	s.handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
}

// caller resolves who calls; a status other than 0 refuses them, for why.
func (s *Server) caller(ctx context.Context, p auth.Principal) (Caller, int, string) {
	c := Caller{Principal: p}
	switch p.Kind {
	case auth.KindUser:
	case auth.KindJob:
		job, err := s.store.GetJob(ctx, p.JobID)
		if err != nil {
			return c, http.StatusForbidden, "unknown job"
		}
		if !jobscope.Active(job.Status) {
			return c, http.StatusForbidden, "this job has finished"
		}
		c.Job, c.scope = job, jobscope.For(job.Kind)
	default:
		return c, http.StatusForbidden, "the Brain is for people and for jobs' own identities"
	}
	reader, err := s.readers.ForPrincipal(ctx, p)
	if err != nil {
		s.log.Warn("mcp: reading the caller", "err", err)
		return c, http.StatusInternalServerError, "internal error"
	}
	c.Reader = reader
	return c, 0, ""
}

// serverFor is the MCP server of one request: the tools and resources its caller may use, and no
// others — a tool a job may not call is not there to be tried.
func (s *Server) serverFor(r *http.Request) *sdk.Server {
	c, ok := r.Context().Value(callerKey{}).(Caller)
	if !ok {
		return nil
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: Name, Title: "Gator Brain", Version: version.Version}, &sdk.ServerOptions{
		Instructions: "What this team knows (the Brain), and the job's own task and conversation. " +
			"Cite an entry you rely on with its tag, [brain:<id>].",
	})
	for _, t := range s.tools {
		if t.Allowed(c) {
			t.Add(srv, c, s.record)
		}
	}
	for _, res := range s.resources {
		if res.Allowed(c) {
			res.Add(srv, c)
		}
	}
	return srv
}

// record writes a job's call beside the job, as every call of the agent surface is; a person's
// own calls are theirs. A failure to record must not fail the call: the work already happened.
func (s *Server) record(ctx context.Context, c Caller, tool string, fields map[string]any) {
	if !c.IsJob() {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	detail, _ := json.Marshal(fields)
	if err := s.store.RecordAgentCall(ctx, db.RecordAgentCallParams{JobID: c.Job.ID, TaskID: c.Job.TaskID,
		Command: "mcp " + tool, Detail: detail}); err != nil {
		s.log.Warn("mcp: recording an agent call", "job", brain.UUIDString(c.Job.ID), "tool", tool, "err", err)
	}
}

func refuse(w http.ResponseWriter, status int, why string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": why})
}
