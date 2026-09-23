package api

import (
	"net/http"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// A job's prompt is assembled from documents the project has: what it decided about itself,
// how this workspace builds software, the part of the product being changed, and what earlier
// phases produced. The size of that package has been recorded since M5, which answers "how
// much" and never "what" — and never why something a person expected to be there was missing.
// These two readings answer that: one for a job that ran, one for a job about to.

// ListJobContext answers what this job was handed, as it was handed over.
func (s *Server) ListJobContext(w http.ResponseWriter, r *http.Request, jobId gen.JobId) {
	job, ok := s.requireJob(w, r, jobId, auth.RoleViewer)
	if !ok {
		return
	}
	rows, err := db.New(s.Pool).ListJobContext(r.Context(), job.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := gen.ContextPackage{Documents: make([]gen.ContextDocument, 0, len(rows))}
	for _, d := range rows {
		out.Documents = append(out.Documents, gen.ContextDocument{
			Kind: d.Kind, Phase: &d.Phase, Title: d.Title, Origin: &d.Origin, Body: &d.Body,
			Bytes: len(d.Body), FullBytes: int(d.FullBytes), LeftOut: &d.LeftOut, Dropped: d.Dropped,
		})
		if d.Dropped {
			out.Dropped++
		} else {
			out.Bytes += len(d.Body)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// PreviewTaskContext assembles the package a job started now would carry. It reads what the
// project knows today, so it is a forecast, not a record — a job that ran yesterday is read
// through ListJobContext, which is what actually went.
func (s *Server) PreviewTaskContext(w http.ResponseWriter, r *http.Request, taskId gen.TaskId, params gen.PreviewTaskContextParams) {
	if _, ok := s.requireTask(w, r, fromUUID(taskId), auth.RoleViewer); !ok {
		return
	}
	role := ""
	if params.Role != nil {
		role = *params.Role
	}
	if role == "" {
		d, err := s.Process.Detail(r.Context(), fromUUID(taskId))
		if err != nil {
			s.fail(w, err)
			return
		}
		role = d.Phase.Role
		if role == "" {
			writeError(w, http.StatusBadRequest, "this phase has no agent role; name one with ?role=", "invalid")
			return
		}
	}
	docs, err := s.Process.JobContext(r.Context(), fromUUID(taskId), role)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toContextPackage(docs))
}

func toContextPackage(docs []process.ContextDoc) gen.ContextPackage {
	out := gen.ContextPackage{Documents: make([]gen.ContextDocument, 0, len(docs))}
	for _, d := range docs {
		doc := d
		out.Documents = append(out.Documents, gen.ContextDocument{
			Kind: doc.Kind, Phase: &doc.Phase, Title: doc.Title, Origin: &doc.Origin, Body: &doc.Body,
			Bytes: len(doc.Body), FullBytes: doc.FullBytes, LeftOut: &doc.Left, Dropped: doc.Dropped,
		})
		if doc.Dropped {
			out.Dropped++
		} else {
			out.Bytes += len(doc.Body)
		}
	}
	return out
}
