package api

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/store/db"
)

// visibilityTTL is how long a connection trusts an answer about whether its caller may read a
// project. Someone removed from a project stops hearing about it within this long.
const visibilityTTL = 15 * time.Second

// eventFilter decides, for one WebSocket connection, which hub events its caller may see. The
// hub routes by topic and knows nothing of callers, so every topic ("inbox", "task:<id>",
// "job:<id>", "*") would otherwise carry every project's events to anyone signed in. An event
// that belongs to a project reaches the caller only if they may read that project, by the same
// rule as the REST API and "project:<id>": project members (viewer and up), workspace admins,
// runners other than a one-job cloud runner, and a plugin for its own project. An event about
// something whose project cannot be found is withheld. Events that belong to no project
// (runners coming and going) are delivered as before.
type eventFilter struct {
	authz auth.Authorizer
	q     *db.Queries
	p     auth.Principal
	now   func() time.Time

	readable map[string]readableAt // project id -> answer
	owners   map[string]string     // aggregate:id -> project id ("" when none)
}

type readableAt struct {
	ok bool
	at time.Time
}

func newEventFilter(s *Server, p auth.Principal) *eventFilter {
	return &eventFilter{authz: s.Authz, q: db.New(s.Pool), p: p, now: time.Now,
		readable: map[string]readableAt{}, owners: map[string]string{}}
}

// projectAggregates are the aggregates that live inside a project. An event about one of them
// whose project is unknown is not delivered.
var projectAggregates = map[string]bool{"task": true, "job": true, "release": true, "incident": true, "project": true}

// allow reports whether the caller may see e.
func (f *eventFilter) allow(ctx context.Context, e events.Event) bool {
	if f.p.IsWorkspaceAdmin() {
		return true
	}
	project := e.ProjectID
	if project == "" && projectAggregates[e.Aggregate] {
		var ok bool
		if project, ok = f.owner(ctx, e.Aggregate, e.AggregateID); !ok || project == "" {
			return false
		}
	}
	if project == "" {
		return true
	}
	return f.mayRead(ctx, project)
}

// owner resolves the project an aggregate belongs to. Tasks, jobs, releases and incidents
// never move between projects, so the answer is kept for the connection's lifetime.
func (f *eventFilter) owner(ctx context.Context, aggregate, id string) (string, bool) {
	key := aggregate + ":" + id
	if p, ok := f.owners[key]; ok {
		return p, true
	}
	var uid pgtype.UUID
	if err := uid.Scan(id); err != nil {
		return "", false
	}
	pid, err := f.q.AggregateProject(ctx, db.AggregateProjectParams{Aggregate: aggregate, ID: uid})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", false // try again on the next event rather than remember a failure
	}
	project := ""
	if pid.Valid {
		project = uuidString(pid)
	}
	f.owners[key] = project
	return project, true
}

// mayRead answers whether the caller may read project, from a short-lived cache.
func (f *eventFilter) mayRead(ctx context.Context, project string) bool {
	if r, ok := f.readable[project]; ok && f.now().Sub(r.at) < visibilityTTL {
		return r.ok
	}
	var pid pgtype.UUID
	if err := pid.Scan(project); err != nil {
		return false
	}
	role, err := f.authz.ProjectRole(ctx, f.p, pid)
	if err != nil {
		return false // fail closed, and do not cache a database hiccup
	}
	ok := role >= auth.RoleViewer
	f.readable[project] = readableAt{ok: ok, at: f.now()}
	return ok
}
