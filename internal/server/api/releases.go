package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// releasePage is how much history a screen shows before someone asks for more; a release list
// is read to answer "what is out there now", not to audit a year.
const releasePage = 50

// ListReleases answers what reached each environment and whether it is still being watched.
func (s *Server) ListReleases(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	id := fromUUID(projectId)
	if _, ok := s.requireProject(w, r, id, auth.RoleViewer); !ok {
		return
	}
	q := db.New(s.Pool)
	rows, err := q.ListReleases(r.Context(), db.ListReleasesParams{ProjectID: id, Limit: releasePage})
	if err != nil {
		s.fail(w, err)
		return
	}
	now := time.Now()
	out := make([]gen.Release, 0, len(rows))
	for _, rel := range rows {
		open, err := q.CountOpenIncidentsForRelease(r.Context(), rel.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, toRelease(rel, int(open), now))
	}
	writeJSON(w, http.StatusOK, out)
}

// ListIncidents answers what production said went wrong, open ones first.
func (s *Server) ListIncidents(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	id := fromUUID(projectId)
	if _, ok := s.requireProject(w, r, id, auth.RoleViewer); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListIncidents(r.Context(), db.ListIncidentsParams{ProjectID: id, Limit: releasePage})
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.Incident, 0, len(rows))
	for _, inc := range rows {
		out = append(out, toIncident(inc))
	}
	writeJSON(w, http.StatusOK, out)
}

func toRelease(r db.Release, openIncidents int, now time.Time) gen.Release {
	out := gen.Release{
		Id: toUUID(r.ID), TaskId: toUUIDPtr(r.TaskID), Version: r.Version, CommitSha: &r.CommitSha,
		Url: &r.Url, Environment: r.Environment, DeployedAt: r.DeployedAt.Time,
		OpenIncidents: &openIncidents,
		// Watching is the answer a screen needs: the window is open and nothing has been said.
		Watching: !r.SettledAt.Valid && r.ObservationUntil.Valid && r.ObservationUntil.Time.After(now),
	}
	out.ObservationUntil = timePtr(r.ObservationUntil)
	out.SettledAt = timePtr(r.SettledAt)
	return out
}

func toIncident(i db.Incident) gen.Incident {
	count := int(i.Count)
	out := gen.Incident{
		Id: toUUID(i.ID), TaskId: toUUIDPtr(i.TaskID), ReleaseId: toUUIDPtr(i.ReleaseID),
		Fingerprint: i.Fingerprint, Title: i.Title, Severity: gen.IncidentSeverity(i.Severity),
		Url: &i.Url, Count: count, FirstSeenAt: i.FirstSeenAt.Time, LastSeenAt: i.LastSeenAt.Time,
	}
	out.ClosedAt = timePtr(i.ClosedAt)
	return out
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
