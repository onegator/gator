package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// The agent surface: what a running job may say back to Gator. It is deliberately a short list
// of its own, rather than the whole API opened to a job's token — an agent reads words written
// by people outside this workspace, so whatever it can reach is whatever those words can reach.
// Every call here is about the job's own task, and every call is written into the job's events,
// where a person watching the work sees it.

// agentJob resolves the job behind a job token, or answers the caller and returns false.
func (s *Server) agentJob(w http.ResponseWriter, r *http.Request) (db.Job, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.Kind != auth.KindJob || !p.JobID.Valid {
		writeError(w, http.StatusForbidden, "this is for a job's own identity", "forbidden")
		return db.Job{}, false
	}
	job, err := db.New(s.Pool).GetJob(r.Context(), p.JobID)
	if err != nil {
		s.fail(w, err)
		return db.Job{}, false
	}
	switch job.Status {
	case "queued", "leased", "running", "stalled":
	default:
		// The token is revoked when a job ends, so this is a race, not a hole. Say so plainly.
		writeError(w, http.StatusForbidden, "this job has finished", "forbidden")
		return db.Job{}, false
	}
	return job, true
}

// note records what the agent said back, beside the job it said it from. Plugins have had this
// since M3; an agent's calls are no less worth reading, and a failure to record one must not
// fail the call — the work already happened.
func (s *Server) note(r *http.Request, job db.Job, command string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	detail, _ := json.Marshal(fields)
	if err := db.New(s.Pool).RecordAgentCall(r.Context(), db.RecordAgentCallParams{
		JobID: job.ID, TaskID: job.TaskID, Command: command, Detail: detail}); err != nil && s.Log != nil {
		s.Log.Warn("recording an agent call", "job", job.ID, "command", command, "err", err)
	}
}

// agentActor is how the work shows up in the audit: a runner acting for this job. The model is
// not an identity Gator can vouch for; the job is.
func agentActor(job db.Job) process.Actor {
	return process.Actor{Kind: process.ActorRunner, ID: job.RunnerID}
}

func taskLink(id pgtype.UUID) string { return "gator://tasks/" + toUUID(id).String() }

// AgentTask answers what this job is about.
func (s *Server) AgentTask(w http.ResponseWriter, r *http.Request) {
	job, ok := s.agentJob(w, r)
	if !ok {
		return
	}
	d, err := s.Process.Detail(r.Context(), job.TaskID)
	if err != nil {
		s.fail(w, err)
		return
	}
	artifacts, err := db.New(s.Pool).ListArtifacts(r.Context(), job.TaskID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := gen.AgentTask{
		JobId: toUUID(job.ID), Task: toTask(d.Task), Phase: job.Phase, Role: job.Role,
		Artifacts: make([]gen.Artifact, 0, len(artifacts)),
	}
	gate := toGate(d.Gate, d.Phase)
	out.Gate = &gate
	for _, a := range artifacts {
		out.Artifacts = append(out.Artifacts, toArtifact(a))
	}
	s.note(r, job, "task show", nil)
	writeJSON(w, http.StatusOK, out)
}

// AgentBlocked stops the task with the agent's reason. An agent that cannot go on should say
// so: the alternative is an hour of guessing, paid for, ending in a report nobody can use.
func (s *Server) AgentBlocked(w http.ResponseWriter, r *http.Request) {
	job, ok := s.agentJob(w, r)
	if !ok {
		return
	}
	var in gen.AgentReason
	if !decode(w, r, &in) {
		return
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		writeError(w, http.StatusBadRequest, "say why, in a sentence a person can act on", "invalid")
		return
	}
	if err := s.Process.Block(r.Context(), job.TaskID, agentActor(job), "the agent stopped: "+reason, "agent"); err != nil {
		if errors.Is(err, process.ErrTaskClosed) {
			writeError(w, http.StatusBadRequest, "this task is closed", "invalid")
			return
		}
		s.fail(w, err)
		return
	}
	s.note(r, job, "task blocked", map[string]any{"reason": reason})
	writeJSON(w, http.StatusOK, gen.AgentWrite{Ok: true, Id: ptr(toUUID(job.TaskID)), Link: ptr(taskLink(job.TaskID)),
		Detail: ptr("the task is blocked and a person has been asked")})
}

// AgentQuestion asks a person and stops there. The question blocks the gate, so it reaches the
// inbox the same way every other decision does — and, like every other block, a phase timeout
// eventually calls it out rather than letting it wait for ever.
func (s *Server) AgentQuestion(w http.ResponseWriter, r *http.Request) {
	job, ok := s.agentJob(w, r)
	if !ok {
		return
	}
	var in gen.AgentReason
	if !decode(w, r, &in) {
		return
	}
	question := strings.TrimSpace(in.Reason)
	if question == "" {
		writeError(w, http.StatusBadRequest, "ask something", "invalid")
		return
	}
	if err := s.Process.Block(r.Context(), job.TaskID, agentActor(job), "the agent asks: "+question, "agent"); err != nil {
		if errors.Is(err, process.ErrTaskClosed) {
			writeError(w, http.StatusBadRequest, "this task is closed", "invalid")
			return
		}
		s.fail(w, err)
		return
	}
	s.note(r, job, "ask", map[string]any{"question": question})
	writeJSON(w, http.StatusOK, gen.AgentWrite{Ok: true, Id: ptr(toUUID(job.TaskID)), Link: ptr(taskLink(job.TaskID)),
		Detail: ptr("the question is in the inbox; answer it by unblocking the task")})
}

// AgentPutArtifact records a document for the job's phase before the job ends, so work survives
// a job that later fails.
func (s *Server) AgentPutArtifact(w http.ResponseWriter, r *http.Request) {
	job, ok := s.agentJob(w, r)
	if !ok {
		return
	}
	var in gen.AgentArtifact
	if !decode(w, r, &in) {
		return
	}
	typ, content := strings.TrimSpace(in.Type), strings.TrimSpace(in.Content)
	if typ == "" || content == "" {
		writeError(w, http.StatusBadRequest, "a document needs a type and content", "invalid")
		return
	}
	a, err := db.New(s.Pool).CreateArtifact(r.Context(), db.CreateArtifactParams{
		TaskID: job.TaskID, Phase: job.Phase, Type: typ, Content: &content})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.note(r, job, "artifact put", map[string]any{"type": typ, "version": a.Version})
	writeJSON(w, http.StatusOK, gen.AgentWrite{Ok: true, Id: ptr(toUUID(a.ID)), Link: ptr(taskLink(job.TaskID)),
		Detail: ptr(fmt.Sprintf("%s v%d recorded on %s", typ, a.Version, job.Phase))})
}

// AgentProposeProduct proposes what the work taught the product. Always proposed: a person
// decides what the product knows about itself, and an agent reading outside words must not be
// able to write into the documents every later prompt carries.
func (s *Server) AgentProposeProduct(w http.ResponseWriter, r *http.Request) {
	job, ok := s.agentJob(w, r)
	if !ok {
		return
	}
	var in gen.AgentProduct
	if !decode(w, r, &in) {
		return
	}
	title, content := strings.TrimSpace(in.Title), strings.TrimSpace(in.Content)
	if title == "" || content == "" {
		writeError(w, http.StatusBadRequest, "an entry needs a title and content", "invalid")
		return
	}
	entry, err := db.New(s.Pool).CreateProductEntry(r.Context(), db.CreateProductEntryParams{
		ProjectID: job.ProjectID, Kind: string(in.Kind), Title: title, Content: content,
		Status: "proposed", SourceTaskID: job.TaskID, CreatedByKind: "runner",
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.note(r, job, "product propose", map[string]any{"kind": string(in.Kind), "title": title})
	writeJSON(w, http.StatusOK, gen.AgentWrite{Ok: true, Id: ptr(toUUID(entry.ID)), Link: ptr(taskLink(job.TaskID)),
		Detail: ptr("proposed; a person approves it before any prompt carries it")})
}

// AgentCatalogue answers what the project is made of.
func (s *Server) AgentCatalogue(w http.ResponseWriter, r *http.Request) {
	job, ok := s.agentJob(w, r)
	if !ok {
		return
	}
	out, err := s.components(r, job.ProjectID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.note(r, job, "catalogue show", nil)
	writeJSON(w, http.StatusOK, out)
}

// AgentKnowledge searches this project's documents for words the job's package did not carry.
func (s *Server) AgentKnowledge(w http.ResponseWriter, r *http.Request, params gen.AgentKnowledgeParams) {
	job, ok := s.agentJob(w, r)
	if !ok {
		return
	}
	query := strings.TrimSpace(params.Q)
	if query == "" {
		writeError(w, http.StatusBadRequest, "search for something", "invalid")
		return
	}
	limit := 10
	if params.Limit != nil && *params.Limit > 0 {
		limit = *params.Limit
	}
	rows, err := db.New(s.Pool).SearchProjectDocuments(r.Context(), db.SearchProjectDocumentsParams{
		ProjectID: job.ProjectID, Query: query, MaxRows: int32(limit)})
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.AgentDocument, 0, len(rows))
	for _, row := range rows {
		out = append(out, gen.AgentDocument{Kind: row.Kind, Title: fmt.Sprint(row.Title), Body: row.Content})
	}
	s.note(r, job, "knowledge search", map[string]any{"query": query, "found": len(out)})
	writeJSON(w, http.StatusOK, out)
}

func ptr[T any](v T) *T { return &v }
