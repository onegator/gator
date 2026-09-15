package api

import (
	"encoding/json"
	"errors"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/store/db"
)

func (s *Server) requireJob(w http.ResponseWriter, r *http.Request, jobID gen.JobId, min auth.Role) (db.Job, bool) {
	job, err := db.New(s.Pool).GetJob(r.Context(), fromUUID(jobID))
	if err != nil {
		s.fail(w, err)
		return db.Job{}, false
	}
	if _, ok := s.requireProject(w, r, job.ProjectID, min); !ok {
		return db.Job{}, false
	}
	return job, true
}

func (s *Server) ListTaskJobs(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleViewer); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListJobsByTask(r.Context(), fromUUID(taskId))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.Job, 0, len(rows))
	for _, j := range rows {
		out = append(out, toJob(j))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) CreateTaskJob(w http.ResponseWriter, r *http.Request, taskId gen.TaskId) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleMember); !ok {
		return
	}
	var in gen.NewJob
	if !decode(w, r, &in) {
		return
	}
	nj := runners.NewJob{Backend: deref(in.Backend), Model: deref(in.Model), Instruction: deref(in.Instruction), Role: deref(in.Role), CreatedBy: ActorFromContext(r.Context())}
	if in.MaxAttempts != nil {
		nj.MaxAttempts = *in.MaxAttempts
	}
	if in.TimeoutSeconds != nil {
		nj.Bounds.TimeoutSeconds = *in.TimeoutSeconds
	}
	if in.MaxCostUsd != nil {
		nj.Bounds.MaxCostUSD = *in.MaxCostUsd
	}
	if in.MaxToolCalls != nil {
		nj.Bounds.MaxToolCalls = *in.MaxToolCalls
	}
	job, err := s.Runners.CreateJob(r.Context(), fromUUID(taskId), nj)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toJob(job))
}

func (s *Server) GetJob(w http.ResponseWriter, r *http.Request, jobId gen.JobId) {
	job, ok := s.requireJob(w, r, jobId, auth.RoleViewer)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toJob(job))
}

func (s *Server) ListJobEvents(w http.ResponseWriter, r *http.Request, jobId gen.JobId, params gen.ListJobEventsParams) {
	if _, ok := s.requireJob(w, r, jobId, auth.RoleViewer); !ok {
		return
	}
	after, limit := int64(0), int32(200)
	if params.After != nil {
		after = *params.After
	}
	if params.Limit != nil {
		limit = int32(*params.Limit)
	}
	rows, err := db.New(s.Pool).ListJobEvents(r.Context(), db.ListJobEventsParams{JobID: fromUUID(jobId), Seq: after, Limit: limit})
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.JobEvent, 0, len(rows))
	for _, e := range rows {
		out = append(out, gen.JobEvent{Seq: e.Seq, Type: e.Type, At: e.At.Time, Payload: objectOf(e.Payload)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) StopJob(w http.ResponseWriter, r *http.Request, jobId gen.JobId) {
	if _, ok := s.requireJob(w, r, jobId, auth.RoleMember); !ok {
		return
	}
	var in gen.StopJob
	_ = json.NewDecoder(r.Body).Decode(&in)
	if err := s.Runners.StopJob(r.Context(), fromUUID(jobId), deref(in.Reason)); err != nil {
		s.fail(w, err)
		return
	}
	job, err := db.New(s.Pool).GetJob(r.Context(), fromUUID(jobId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toJob(job))
}

func (s *Server) SteerJob(w http.ResponseWriter, r *http.Request, jobId gen.JobId) {
	if _, ok := s.requireJob(w, r, jobId, auth.RoleMember); !ok {
		return
	}
	var in gen.SteerJob
	if !decode(w, r, &in) {
		return
	}
	if in.Message == "" {
		writeError(w, http.StatusBadRequest, "message is required", "invalid")
		return
	}
	if err := s.Runners.SteerJob(r.Context(), fromUUID(jobId), in.Message); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) ListRunners(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if p.Kind != auth.KindUser {
		writeError(w, http.StatusForbidden, "runner list is for people", "forbidden")
		return
	}
	rows, err := db.New(s.Pool).ListRunners(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.RunnerInfo, 0, len(rows))
	for _, rn := range rows {
		out = append(out, toRunner(rn, s.Runners.Connected(rn.ID)))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) RequestRunnerLogin(w http.ResponseWriter, r *http.Request, runnerId gen.RunnerId) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, "backend logins require workspace admin", "forbidden")
		return
	}
	var in gen.LoginRequest
	if !decode(w, r, &in) {
		return
	}
	if err := s.Runners.RequestLogin(fromUUID(runnerId), in.Backend); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func toJob(j db.Job) gen.Job {
	out := gen.Job{
		Id: toUUID(j.ID), TaskId: toUUID(j.TaskID), ProjectId: toUUID(j.ProjectID), Phase: j.Phase, Role: j.Role,
		Backend: j.Backend, Instruction: j.Instruction, Status: gen.JobStatus(j.Status), RunnerId: toUUIDPtr(j.RunnerID),
		Attempts: int(j.Attempts), MaxAttempts: int(j.MaxAttempts), StopReason: j.StopReason, CreatedAt: j.CreatedAt.Time,
	}
	if j.Model != "" {
		out.Model = &j.Model
	}
	if j.LeaseExpiresAt.Valid {
		v := j.LeaseExpiresAt.Time
		out.LeaseExpiresAt = &v
	}
	if j.LastEventAt.Valid {
		v := j.LastEventAt.Time
		out.LastEventAt = &v
	}
	if j.StartedAt.Valid {
		v := j.StartedAt.Time
		out.StartedAt = &v
	}
	if j.FinishedAt.Valid {
		v := j.FinishedAt.Time
		out.FinishedAt = &v
	}
	if len(j.Receipt) > 0 {
		m := objectOf(j.Receipt)
		out.Receipt = &m
	}
	return out
}

func toRunner(r db.Runner, connected bool) gen.RunnerInfo {
	var caps proto.Capabilities
	_ = json.Unmarshal(r.Capabilities, &caps)
	if caps.Backends == nil {
		caps.Backends = []string{}
	}
	if caps.Projects == nil {
		caps.Projects = []string{}
	}
	authState := map[string]string{}
	_ = json.Unmarshal(r.AuthState, &authState)
	out := gen.RunnerInfo{
		Id: toUUID(r.ID), Name: r.Name, Location: r.Location, Status: gen.RunnerInfoStatus(r.Status), Connected: connected,
		Capabilities: gen.RunnerCapabilities{Backends: caps.Backends, MaxParallel: caps.MaxParallel, Projects: caps.Projects},
		AuthState:    authState, ProtocolVersion: int(r.ProtocolVersion), BinaryVersion: r.BinaryVersion,
	}
	if r.LastHeartbeatAt.Valid {
		v := r.LastHeartbeatAt.Time
		out.LastHeartbeatAt = &v
	}
	return out
}

// objectOf decodes JSON into an object; a non-object value is wrapped as {"value": …}.
func objectOf(b []byte) map[string]interface{} {
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err == nil && m != nil {
		return m
	}
	var v interface{}
	_ = json.Unmarshal(b, &v)
	return map[string]interface{}{"value": v}
}

var _ = openapi_types.UUID{}

func runnerErrStatus(err error) (int, bool) {
	switch {
	case errors.Is(err, runners.ErrInvalidJob):
		return http.StatusBadRequest, true
	case errors.Is(err, runners.ErrBudgetExceeded), errors.Is(err, runners.ErrJobFinished), errors.Is(err, runners.ErrRunnerOffline), errors.Is(err, runners.ErrJobNotActive):
		return http.StatusConflict, true
	}
	return 0, false
}
