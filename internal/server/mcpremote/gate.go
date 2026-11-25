package mcpremote

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/jobscope"
	"github.com/onegator/gator/internal/server/store/db"
)

// Grant is what one job may reach of one server.
type Grant struct {
	Job    db.Job
	Agent  db.Agent
	Server db.McpServer
	// Tools narrow the server's allowlist to what the agent was given (its server/tool entries);
	// empty: everything the allowlist holds.
	Tools []string
	// ReadOnly holds a chat job to the tools the server marks read-only: chat has no tools that
	// write, and a remote tool is no exception.
	ReadOnly bool
}

// Gate decides who reaches a server.
type Gate interface {
	Authorize(ctx context.Context, p auth.Principal, server string) (Grant, error)
}

// gateStore is what the gate reads. *db.Queries is one.
type gateStore interface {
	GetJob(ctx context.Context, id pgtype.UUID) (db.Job, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	MCPServerFor(ctx context.Context, arg db.MCPServerForParams) (db.McpServer, error)
}

// JobGate lets a job's own identity through to a server its agent was given: the job still
// running, its kind allowed the proxy (jobscope: a summary is not), its agent naming the server
// in its tools, and the server, the project's or the workspace's, switched on.
type JobGate struct {
	Store gateStore
}

// Authorize implements Gate.
func (g JobGate) Authorize(ctx context.Context, p auth.Principal, server string) (Grant, error) {
	if p.Kind != auth.KindJob {
		return Grant{}, ErrForbidden
	}
	job, err := g.Store.GetJob(ctx, p.JobID)
	if err != nil || !jobscope.Active(job.Status) || !jobscope.For(job.Kind)[jobscope.MCPProxy] || !job.AgentID.Valid {
		return Grant{}, ErrForbidden
	}
	agent, err := g.Store.GetAgent(ctx, job.AgentID)
	if err != nil || agent.DeletedAt.Valid {
		return Grant{}, ErrForbidden
	}
	whole, tools, ok := Granted(agent, server)
	if !ok {
		return Grant{}, ErrForbidden
	}
	srv, err := g.Store.MCPServerFor(ctx, db.MCPServerForParams{Name: server, ProjectID: job.ProjectID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !srv.Enabled) {
		return Grant{}, ErrNotFound
	}
	if err != nil {
		return Grant{}, err
	}
	gr := Grant{Job: job, Agent: agent, Server: srv, ReadOnly: job.Tier == "chat"}
	if !whole {
		gr.Tools = tools
	}
	return gr, nil
}

// Granted reads an agent's tools for one server: whether it has the server whole, the tools it
// was given one by one otherwise, and whether it has the server at all.
func Granted(a db.Agent, server string) (whole bool, tools []string, ok bool) {
	var entries []string
	_ = json.Unmarshal(a.Tools, &entries)
	for _, e := range entries {
		name, tool, one := strings.Cut(e, "/")
		if name != server {
			continue
		}
		ok = true
		if !one {
			whole = true
		} else if !slices.Contains(tools, tool) {
			tools = append(tools, tool)
		}
	}
	return whole, tools, ok
}

// GrantedServers lists the server names an agent was given.
func GrantedServers(a db.Agent) []string {
	var entries, out []string
	_ = json.Unmarshal(a.Tools, &entries)
	for _, e := range entries {
		name, _, _ := strings.Cut(e, "/")
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}
