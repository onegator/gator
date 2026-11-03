package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/plugins"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// ListInbox answers what waits for a person. Without all=true it is the task inbox it always
// was. With it, everything a person has to decide is in the one answer, so a client reads
// decisions from one place: agent proposals for the catalogue and the product context,
// runners whose backend lost its login, plugins Gator switched off, and stalled jobs.
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
	all := params.All != nil && *params.All
	var owner pgtype.UUID
	if params.Mine != nil && *params.Mine {
		owner = p.UserID
	}
	rows, err := s.Process.Inbox(r.Context(), process.InboxFilter{ProjectID: pid, OwnerID: owner, All: all})
	if err != nil {
		s.fail(w, err)
		return
	}
	readable := func(project pgtype.UUID) bool { return visible == nil || visible[project.Bytes] }

	var tripped map[string]db.ListTrippedProjectPluginsRow
	if all {
		if tripped, err = s.trippedPlugins(r.Context(), pid, readable); err != nil {
			s.fail(w, err)
			return
		}
	}
	out := make([]gen.Decision, 0, len(rows))
	for _, d := range rows {
		if !readable(d.Task.ProjectID) {
			continue
		}
		dec := taskDecision(d)
		if d.Reason == process.ReasonAlert {
			// The alert Gator raised for a switched-off plugin is that plugin's decision: one
			// row, which can switch it back on, rather than an alert and a plugin side by side.
			for key, tp := range tripped {
				if tp.ProjectID == d.Task.ProjectID && d.Task.Title == plugins.DisabledAlertTitle(tp.PluginName) {
					dec = pluginDecision(tp, &dec)
					delete(tripped, key)
					break
				}
			}
		}
		out = append(out, dec)
	}
	if all {
		for _, tp := range tripped {
			out = append(out, pluginDecision(tp, nil))
		}
		more, err := s.proposalDecisions(r.Context(), pid, readable)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, more...)
		// Runners belong to the workspace, and only a workspace admin may ask one to log in.
		if p.IsWorkspaceAdmin() && !owner.Valid {
			logins, err := s.runnerLoginDecisions(r.Context())
			if err != nil {
				s.fail(w, err)
				return
			}
			out = append(out, logins...)
		}
		sortDecisions(out)
	}
	writeJSON(w, http.StatusOK, out)
}

// sortDecisions puts the most urgent first, and of equal urgency the one waiting longest.
func sortDecisions(ds []gen.Decision) {
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].Urgency != ds[j].Urgency {
			return ds[i].Urgency < ds[j].Urgency
		}
		return ds[i].WaitingSince.Before(ds[j].WaitingSince)
	})
}

// taskDecision is the row for a task that waits on a person.
func taskDecision(d process.Decision) gen.Decision {
	t := toTask(d.Task)
	rollbacks := d.Rollbacks
	pid := toUUID(d.Task.ProjectID)
	urgency := int(d.Task.Urgency)
	if stuckReason(d.Reason) && urgency > 2 {
		urgency = 2 // it cannot move by itself, whatever was said when it was written
	}
	summary, actions := taskWords(d)
	return gen.Decision{
		Id: "task:" + uuidString(d.Task.ID), Task: &t, Reason: gen.DecisionReason(d.Reason),
		WaitingSince: d.WaitingSince, Rollbacks: &rollbacks, Urgency: urgency,
		Summary: summary, Actions: actions, JobId: toUUIDPtr(d.JobID),
		Subject: gen.DecisionSubject{Kind: gen.DecisionSubjectKindTask, Id: uuidString(d.Task.ID), ProjectId: &pid, Name: d.Task.Title},
	}
}

func stuckReason(r process.DecisionReason) bool {
	switch r {
	case process.ReasonBlocked, process.ReasonJobFailed, process.ReasonNoRunner, process.ReasonJobStalled:
		return true
	}
	return false
}

// taskWords says what happened to the task, and what can be done about it from the inbox.
func taskWords(d process.Decision) (string, []gen.DecisionAction) {
	phase := strings.ReplaceAll(d.Task.Phase, "_", " ")
	open := gen.DecisionActionOpen
	switch d.Reason {
	case process.ReasonBlocked:
		if d.Task.BlockedReason != nil && *d.Task.BlockedReason != "" {
			return "Blocked: " + *d.Task.BlockedReason, []gen.DecisionAction{open}
		}
		return "Blocked in " + phase + ".", []gen.DecisionAction{open}
	case process.ReasonExternal:
		return "Raised outside the workspace. Nothing runs on it until you let it in.",
			[]gen.DecisionAction{gen.DecisionActionAdmit, gen.DecisionActionClose, open}
	case process.ReasonAgentsNotAllowed:
		return "An agent is ready to start once you allow it.",
			[]gen.DecisionAction{gen.DecisionActionAllowAgents, open}
	case process.ReasonRequirementsChanged:
		return "The requirements changed after work in " + phase + " started.", []gen.DecisionAction{open}
	case process.ReasonNoRunner:
		return "No runner can take the job in " + phase + ".", []gen.DecisionAction{open}
	case process.ReasonJobFailed:
		return "The agent's job in " + phase + " failed.", []gen.DecisionAction{gen.DecisionActionRetry, open}
	case process.ReasonJobStalled:
		return "The agent in " + phase + " has gone quiet: its job runs but says nothing.",
			[]gen.DecisionAction{gen.DecisionActionStopJob, open}
	case process.ReasonIdea:
		return "A new idea. Take it on, or drop it.", []gen.DecisionAction{gen.DecisionActionApprove, gen.DecisionActionClose, open}
	case process.ReasonAlert:
		return d.Task.Title + ".", []gen.DecisionAction{gen.DecisionActionClose, open}
	}
	if phase == "" {
		return "Waits for your approval.", []gen.DecisionAction{gen.DecisionActionApprove, open}
	}
	return strings.ToUpper(phase[:1]) + phase[1:] + " waits for your approval.",
		[]gen.DecisionAction{gen.DecisionActionApprove, open}
}

// trippedPlugins are the plugins Gator switched off in projects the caller may read, by
// project and name.
func (s *Server) trippedPlugins(ctx context.Context, pid pgtype.UUID, readable func(pgtype.UUID) bool) (map[string]db.ListTrippedProjectPluginsRow, error) {
	rows, err := db.New(s.Pool).ListTrippedProjectPlugins(ctx, pid)
	if err != nil {
		return nil, err
	}
	out := map[string]db.ListTrippedProjectPluginsRow{}
	for _, r := range rows {
		if readable(r.ProjectID) {
			out[uuidString(r.ProjectID)+"/"+r.PluginName] = r
		}
	}
	return out, nil
}

// pluginDecision is the row for a plugin Gator switched off. alert, when there is one, is the
// alert task raised for it: closing that acknowledges it.
func pluginDecision(tp db.ListTrippedProjectPluginsRow, alert *gen.Decision) gen.Decision {
	pid := toUUID(tp.ProjectID)
	reason := ""
	if tp.DisabledReason != nil {
		reason = *tp.DisabledReason
	}
	d := gen.Decision{
		Id: "plugin:" + uuidString(tp.ProjectID) + ":" + tp.PluginName, Reason: gen.DecisionReasonPluginDisabled,
		WaitingSince: tp.UpdatedAt.Time, Urgency: 2,
		Summary: "Gator switched the " + tp.PluginName + " plugin off: " + reason,
		Actions: []gen.DecisionAction{gen.DecisionActionEnablePlugin, gen.DecisionActionOpen},
		Subject: gen.DecisionSubject{Kind: gen.DecisionSubjectKindPlugin, Id: tp.PluginName, ProjectId: &pid, Name: tp.PluginName},
	}
	if alert != nil {
		d.Task, d.WaitingSince = alert.Task, alert.WaitingSince
		d.Actions = append(d.Actions, gen.DecisionActionClose)
	}
	return d
}

// proposalDecisions are the catalogue components and product entries an agent proposed,
// waiting for a person to say yes. They reach no prompt until one does.
func (s *Server) proposalDecisions(ctx context.Context, pid pgtype.UUID, readable func(pgtype.UUID) bool) ([]gen.Decision, error) {
	q := db.New(s.Pool)
	components, err := q.ListProposedComponents(ctx, pid)
	if err != nil {
		return nil, err
	}
	entries, err := q.ListProposedProductEntries(ctx, pid)
	if err != nil {
		return nil, err
	}
	var out []gen.Decision
	for _, c := range components {
		if !readable(c.ProjectID) {
			continue
		}
		project := toUUID(c.ProjectID)
		summary := "Proposed as a component of the catalogue"
		if c.ProposedReason != "" {
			summary += ": " + c.ProposedReason
		} else {
			summary += "."
		}
		out = append(out, gen.Decision{
			Id: "component:" + uuidString(c.ID), Reason: gen.DecisionReasonComponentProposed,
			WaitingSince: c.CreatedAt.Time, Urgency: 4, Summary: summary,
			Actions: []gen.DecisionAction{gen.DecisionActionAcceptComponent, gen.DecisionActionRejectComponent, gen.DecisionActionOpen},
			Subject: gen.DecisionSubject{Kind: gen.DecisionSubjectKindComponent, Id: c.Key, ProjectId: &project, Name: c.Name},
		})
	}
	for _, e := range entries {
		if !readable(e.ProjectID) {
			continue
		}
		project := toUUID(e.ProjectID)
		out = append(out, gen.Decision{
			Id: "product:" + uuidString(e.ID), Reason: gen.DecisionReasonProductProposed,
			WaitingSince: e.CreatedAt.Time, Urgency: 4,
			Summary: "Proposed as a " + e.Kind + " for the product context. Agents read it only once you approve it.",
			Actions: []gen.DecisionAction{gen.DecisionActionApproveEntry, gen.DecisionActionArchiveEntry, gen.DecisionActionOpen},
			Subject: gen.DecisionSubject{Kind: gen.DecisionSubjectKindProductEntry, Id: uuidString(e.ID), ProjectId: &project, Name: e.Title},
		})
	}
	return out, nil
}

// runnerLoginDecisions are the backends that lost their login on a runner that is up. Until a
// person logs in again, the runner takes no job for that backend, and nothing says why.
func (s *Server) runnerLoginDecisions(ctx context.Context) ([]gen.Decision, error) {
	runners, err := db.New(s.Pool).ListRunners(ctx)
	if err != nil {
		return nil, err
	}
	var out []gen.Decision
	for _, r := range runners {
		// A cloud runner carries its login in its environment; one that is offline has a
		// different problem, which the runners screen shows.
		if r.Status == "offline" || r.Placement == string(proto.PlacementCloud) {
			continue
		}
		states := map[string]string{}
		_ = json.Unmarshal(r.AuthState, &states)
		var caps proto.Capabilities
		_ = json.Unmarshal(r.Capabilities, &caps)
		backends := make([]string, 0, len(states))
		for b, st := range states {
			if st == "missing" {
				backends = append(backends, b)
			}
		}
		sort.Strings(backends)
		for _, b := range backends {
			backend := b
			since := r.ConnectedAt.Time
			if since.IsZero() {
				since = time.Now()
			}
			actions := []gen.DecisionAction{gen.DecisionActionOpen}
			summary := r.Name + " has no " + b + " login, so it takes no " + b + " jobs."
			if caps.CanLogin {
				actions = append([]gen.DecisionAction{gen.DecisionActionLoginRunner}, actions...)
			} else {
				summary += " Log in on that machine."
			}
			out = append(out, gen.Decision{
				Id: "runner:" + uuidString(r.ID) + ":" + b, Reason: gen.DecisionReasonRunnerLogin,
				WaitingSince: since, Urgency: 1, Summary: summary, Actions: actions,
				Subject: gen.DecisionSubject{Kind: gen.DecisionSubjectKindRunner, Id: uuidString(r.ID), Name: r.Name, Backend: &backend},
			})
		}
	}
	return out, nil
}
