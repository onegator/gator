package api

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// GetDashboard answers the plugin numbers on the app's home screen in one call: per tracker,
// the issues it brought in that are still open; per monitoring plugin, its open incidents.
// Every plugin switched on in a readable project has an entry, zero included, so a client can
// tell "no errors" from "nothing watches for them".
func (s *Server) GetDashboard(w http.ResponseWriter, r *http.Request, params gen.GetDashboardParams) {
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
	ctx := r.Context()
	q := db.New(s.Pool)
	sources, err := q.ListDashboardSources(ctx, pid)
	if err != nil {
		s.fail(w, err)
		return
	}
	tasks, err := q.CountOpenTasksByOrigin(ctx, pid)
	if err != nil {
		s.fail(w, err)
		return
	}
	incidents, err := q.CountOpenIncidentsByPlugin(ctx, pid)
	if err != nil {
		s.fail(w, err)
		return
	}
	type origin struct {
		project [16]byte
		source  string
	}
	openTasks := make(map[origin]int, len(tasks))
	for _, t := range tasks {
		openTasks[origin{t.ProjectID.Bytes, t.OriginSource}] = int(t.Open)
	}
	openIncidents := make(map[[16]byte]int, len(incidents))
	for _, i := range incidents {
		openIncidents[i.SourcePluginID.Bytes] = int(i.Open)
	}

	out := gen.DashboardCounts{Issues: []gen.PluginCount{}, Errors: []gen.PluginCount{}}
	for _, src := range sources {
		if visible != nil && !visible[src.ProjectID.Bytes] {
			continue
		}
		if src.Tracker {
			out.Issues = append(out.Issues, gen.PluginCount{ProjectId: toUUID(src.ProjectID), Plugin: src.PluginName,
				Open: openTasks[origin{src.ProjectID.Bytes, src.PluginName}]})
		}
		if src.Monitoring {
			out.Errors = append(out.Errors, gen.PluginCount{ProjectId: toUUID(src.ProjectID), Plugin: src.PluginName,
				Open: openIncidents[src.ID.Bytes]})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
