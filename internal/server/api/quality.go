package api

import (
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// ListQuality answers the scorecard: a score per component, the score before it, and the rules
// behind both. The previous score is what makes the number mean anything.
func (s *Server) ListQuality(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	id := fromUUID(projectId)
	if _, ok := s.requireProject(w, r, id, auth.RoleViewer); !ok {
		return
	}
	out, err := s.scorecards(r, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ScoreNow runs the sweep instead of waiting for the daily one — for the person who has just
// fixed something and wants to see it counted.
func (s *Server) ScoreNow(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	id := fromUUID(projectId)
	if _, ok := s.requireProject(w, r, id, auth.RoleAdmin); !ok {
		return
	}
	if s.Quality == nil {
		writeError(w, http.StatusServiceUnavailable, "scorecards are not enabled on this server", "unavailable")
		return
	}
	if err := s.Quality.Sweep(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	out, err := s.scorecards(r, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) scorecards(r *http.Request, projectID pgtype.UUID) ([]gen.Scorecard, error) {
	ctx := r.Context()
	q := db.New(s.Pool)
	components, err := q.ListComponents(ctx, projectID)
	if err != nil {
		return nil, err
	}
	checks, err := q.ListQualityChecks(ctx, projectID)
	if err != nil {
		return nil, err
	}

	// One card per component, plus one for the project as a whole when a rule is about that.
	type card struct {
		id pgtype.UUID
		gen.Scorecard
	}
	cards := map[pgtype.UUID]*card{}
	for _, c := range components {
		name := c.Name
		cards[c.ID] = &card{id: c.ID, Scorecard: gen.Scorecard{
			Component: c.Key, ComponentName: &name, Checks: []gen.QualityCheck{}}}
	}
	for _, ch := range checks {
		c, ok := cards[ch.ComponentID]
		if !ok {
			c = &card{id: ch.ComponentID, Scorecard: gen.Scorecard{Checks: []gen.QualityCheck{}}}
			cards[ch.ComponentID] = c
		}
		detail := ch.Detail
		gc := gen.QualityCheck{Rule: ch.Rule, Source: ch.Source, Status: gen.QualityCheckStatus(ch.Status),
			Detail: &detail, Weight: int(ch.Weight), TaskId: toUUIDPtr(ch.TaskID)}
		gc.FailingSince = timePtr(ch.FailingSince)
		c.Checks = append(c.Checks, gc)
	}

	out := make([]gen.Scorecard, 0, len(cards))
	for _, c := range cards {
		// Two scores: the newest and the one before it. Without the previous number a
		// scorecard tells nobody whether things are getting better.
		history, err := q.QualityScoreHistory(ctx, db.QualityScoreHistoryParams{
			ProjectID: projectID, ComponentID: c.id, Limit: 2})
		if err != nil {
			return nil, err
		}
		if len(history) > 0 {
			c.Earned, c.Possible = int(history[0].Earned), int(history[0].Possible)
			at := history[0].CheckedAt.Time
			c.CheckedAt = &at
		}
		if len(history) > 1 {
			earned, possible := int(history[1].Earned), int(history[1].Possible)
			c.PreviousEarned, c.PreviousPossible = &earned, &possible
		}
		out = append(out, c.Scorecard)
	}
	// Worst first: a scorecard is read to find what needs attention.
	sort.Slice(out, func(i, j int) bool {
		si, sj := share(out[i]), share(out[j])
		if si != sj {
			return si < sj
		}
		return out[i].Component < out[j].Component
	})
	return out, nil
}

func share(c gen.Scorecard) float64 {
	if c.Possible == 0 {
		return 1
	}
	return float64(c.Earned) / float64(c.Possible)
}
