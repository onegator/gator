package mcp

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/agents"
	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/jobscope"
	"github.com/onegator/gator/internal/server/store/db"
)

// finder searches the Brain for a reader. brain.Finder is one.
type finder interface {
	Find(ctx context.Context, r brain.Reader, q brain.Query) ([]brain.Hit, error)
}

// keeper keeps entries. brain.Service is one.
type keeper interface {
	Get(ctx context.Context, r brain.Reader, id pgtype.UUID) (brain.Detail, error)
	Write(ctx context.Context, r brain.Reader, d brain.Draft) (db.BrainEntry, error)
	Visible(ctx context.Context, r brain.Reader, rows []db.BrainEntry) ([]db.BrainEntry, error)
}

// citer records what a job used. brain.Citations is one.
type citer interface {
	Used(ctx context.Context, job pgtype.UUID, how string, entries []pgtype.UUID) error
}

// origins says where a job's proposal comes from: its conversation, and whether that is private.
type origins interface {
	OriginOf(ctx context.Context, job db.Job) (agents.Origin, error)
}

// noOrigin is where a proposal comes from when nothing says: nowhere in particular.
type noOrigin struct{}

func (noOrigin) OriginOf(context.Context, db.Job) (agents.Origin, error) { return agents.Origin{}, nil }

// projects finds a project a person names by id or slug.
type projects interface {
	GetProjectBySlug(ctx context.Context, slug string) (db.Project, error)
}

// BrainTools are the Brain's tools: search, read one entry, propose, cite.
func BrainTools(find finder, keep keeper, cite citer, from origins, named projects) []Tool {
	return []Tool{
		tool[searchIn, searchOut]{name: "brain_search", who: access{command: jobscope.Knowledge, people: true},
			about: "Search what this team knows (decisions, conventions, runbooks, people's shared context) by words. " +
				"Answers the same hits as the Brain screen; cite one you rely on as [brain:<id>].",
			call: func(ctx context.Context, c Caller, in searchIn) (searchOut, map[string]any, error) {
				return search(ctx, c, in, find, cite, named)
			}},
		tool[getIn, getOut]{name: "brain_get", who: access{command: jobscope.Knowledge, people: true},
			about: "Read one entry of the Brain whole: its content, its versions and what it links to.",
			call: func(ctx context.Context, c Caller, in getIn) (getOut, map[string]any, error) {
				return get(ctx, c, in, keep, cite)
			}},
		tool[proposeIn, proposeOut]{name: "brain_propose", who: access{command: jobscope.ProductPropose, people: true},
			about: "Propose something worth remembering (a decision, a lesson, a runbook, a fact). It is always a proposal: " +
				"a person approves it before any prompt carries it.",
			call: func(ctx context.Context, c Caller, in proposeIn) (proposeOut, map[string]any, error) {
				return propose(ctx, c, in, keep, from, named)
			}},
		tool[citeIn, citeOut]{name: "brain_cite", who: access{command: jobscope.Knowledge, people: true},
			about: "Say which Brain entries your work relies on, by id; a person then sees where you got it from.",
			call: func(ctx context.Context, c Caller, in citeIn) (citeOut, map[string]any, error) {
				return citeEntries(ctx, c, in, keep, cite)
			}},
	}
}

type searchIn struct {
	Query   string `json:"query" jsonschema:"words to look for; empty lists the newest"`
	Scope   string `json:"scope,omitempty" jsonschema:"workspace, project, component, user or agent"`
	Kind    string `json:"kind,omitempty" jsonschema:"decision, convention, runbook, lesson, …"`
	Project string `json:"project,omitempty" jsonschema:"a project's id or slug, for a person; a job searches its own"`
	Limit   int    `json:"limit,omitempty" jsonschema:"at most this many, up to 100"`
}

type searchOut struct {
	Hits []brain.Summary `json:"hits"`
}

func search(ctx context.Context, c Caller, in searchIn, find finder, cite citer, named projects) (searchOut, map[string]any, error) {
	q := brain.Query{Text: in.Query, Limit: in.Limit}
	if in.Scope != "" {
		q.Scopes = []string{in.Scope}
	}
	if in.Kind != "" {
		q.Kinds = []string{in.Kind}
	}
	switch {
	case c.IsJob():
		q.Project = c.Job.ProjectID
	case in.Project != "":
		project, err := projectNamed(ctx, named, in.Project)
		if err != nil {
			return searchOut{}, nil, err
		}
		q.Project = project
	}
	hits, err := find.Find(ctx, c.Reader, q)
	if err != nil {
		return searchOut{}, nil, err
	}
	if c.IsJob() {
		if err := cite.Used(ctx, c.Job.ID, brain.HowSearched, brain.IDs(hits)); err != nil {
			return searchOut{}, nil, err
		}
	}
	out := searchOut{Hits: make([]brain.Summary, 0, len(hits))}
	for _, h := range hits {
		out.Hits = append(out.Hits, brain.Summarize(h))
	}
	return out, map[string]any{"query": in.Query, "found": len(hits)}, nil
}

type getIn struct {
	ID string `json:"id" jsonschema:"the entry's id, as in its [brain:<id>] tag"`
}

// entryOut is one entry as a tool shows it.
type entryOut struct {
	ID         string       `json:"id"`
	Title      string       `json:"title"`
	Content    string       `json:"content"`
	Scope      string       `json:"scope"`
	Kind       string       `json:"kind"`
	Status     string       `json:"status"`
	Version    int          `json:"version"`
	Source     brain.Source `json:"source"`
	ValidFrom  string       `json:"validFrom"`
	ValidUntil string       `json:"validUntil,omitempty"`
}

type linkOut struct {
	TargetKind string `json:"targetKind"`
	TargetID   string `json:"targetId"`
	Relation   string `json:"relation"`
}

type getOut struct {
	Entry    entryOut   `json:"entry"`
	Versions []entryOut `json:"versions"`
	Links    []linkOut  `json:"links"`
}

func get(ctx context.Context, c Caller, in getIn, keep keeper, cite citer) (getOut, map[string]any, error) {
	id, ok := brain.ParseUUID(strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(in.ID, "[brain:"), "]"), "brain:"))
	if !ok {
		return getOut{}, nil, refusal("an id is a uuid, as in [brain:<id>]")
	}
	d, err := keep.Get(ctx, c.Reader, id)
	if err != nil {
		return getOut{}, nil, err
	}
	versions, err := keep.Visible(ctx, c.Reader, d.Versions)
	if err != nil {
		return getOut{}, nil, err
	}
	if c.IsJob() {
		if err := cite.Used(ctx, c.Job.ID, brain.HowSearched, []pgtype.UUID{d.Entry.ID}); err != nil {
			return getOut{}, nil, err
		}
	}
	out := getOut{Entry: toEntryOut(d.Entry), Versions: make([]entryOut, 0, len(versions)), Links: make([]linkOut, 0, len(d.Links))}
	for _, v := range versions {
		out.Versions = append(out.Versions, toEntryOut(v))
	}
	for _, l := range d.Links {
		out.Links = append(out.Links, linkOut{TargetKind: l.TargetKind, TargetID: l.TargetID, Relation: l.Relation})
	}
	return out, map[string]any{"id": in.ID}, nil
}

func toEntryOut(e db.BrainEntry) entryOut {
	out := entryOut{ID: brain.UUIDString(e.ID), Title: e.Title, Content: e.Content, Scope: e.Scope, Kind: e.Kind,
		Status: e.Status, Version: int(e.Version), Source: brain.SourceOf(e), ValidFrom: e.ValidFrom.Time.Format("2006-01-02T15:04:05Z07:00")}
	if e.ValidUntil.Valid {
		out.ValidUntil = e.ValidUntil.Time.Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}

type proposeIn struct {
	Kind      string `json:"kind" jsonschema:"decision, lesson, runbook, fact, convention, glossary, …"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Scope     string `json:"scope,omitempty" jsonschema:"project (the default), component or agent"`
	Component string `json:"component,omitempty" jsonschema:"the component's key, for scope component"`
	Project   string `json:"project,omitempty" jsonschema:"a project's id or slug, for a person"`
	Links     []struct {
		TargetKind string `json:"targetKind" jsonschema:"entry, component, task or conversation"`
		TargetID   string `json:"targetId"`
		Relation   string `json:"relation,omitempty" jsonschema:"relates, depends_on, contradicts or implements"`
	} `json:"links,omitempty"`
}

type proposeOut struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func propose(ctx context.Context, c Caller, in proposeIn, keep keeper, from origins, named projects) (proposeOut, map[string]any, error) {
	d := brain.Draft{Scope: in.Scope, Kind: in.Kind, Title: in.Title, Content: in.Content, Component: in.Component, Propose: true}
	if d.Scope == "" {
		d.Scope = brain.ScopeProject
	}
	for _, l := range in.Links {
		d.Links = append(d.Links, brain.Link{TargetKind: l.TargetKind, TargetID: l.TargetID, Relation: l.Relation})
	}
	if c.IsJob() {
		d.Project, d.Task = c.Job.ProjectID, c.Job.TaskID
		if d.Scope == brain.ScopeAgent {
			d.Agent = c.Job.AgentID
		}
		origin, err := from.OriginOf(ctx, c.Job)
		if err != nil {
			return proposeOut{}, nil, err
		}
		d.Conversation, d.Private = origin.Conversation, origin.Private
	} else if in.Project != "" {
		project, err := projectNamed(ctx, named, in.Project)
		if err != nil {
			return proposeOut{}, nil, err
		}
		d.Project = project
	}
	e, err := keep.Write(ctx, c.Reader, d)
	if err != nil {
		return proposeOut{}, nil, err
	}
	return proposeOut{ID: brain.UUIDString(e.ID), Status: e.Status, Detail: "proposed; a person approves it before any prompt carries it"},
		map[string]any{"kind": in.Kind, "title": in.Title}, nil
}

type citeIn struct {
	IDs []string `json:"ids" jsonschema:"the entries your work relies on"`
}

type citeOut struct {
	Recorded int `json:"recorded"`
}

func citeEntries(ctx context.Context, c Caller, in citeIn, keep keeper, cite citer) (citeOut, map[string]any, error) {
	if !c.IsJob() {
		return citeOut{}, nil, nil // a person's own reading is theirs; nothing is recorded
	}
	var ids []pgtype.UUID
	for _, s := range in.IDs {
		id, ok := brain.ParseUUID(strings.TrimSuffix(strings.TrimPrefix(s, "[brain:"), "]"))
		if !ok {
			continue
		}
		// Only what this job may read is cited: an id it was never shown proves nothing.
		if _, err := keep.Get(ctx, c.Reader, id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	if err := cite.Used(ctx, c.Job.ID, brain.HowCited, ids); err != nil {
		return citeOut{}, nil, err
	}
	return citeOut{Recorded: len(ids)}, map[string]any{"cited": len(ids)}, nil
}

// projectNamed is the project a person names, by id or slug.
func projectNamed(ctx context.Context, named projects, s string) (pgtype.UUID, error) {
	if id, ok := brain.ParseUUID(s); ok {
		return id, nil
	}
	if named == nil {
		return pgtype.UUID{}, refusal("name the project by its id")
	}
	p, err := named.GetProjectBySlug(ctx, s)
	if err != nil {
		return pgtype.UUID{}, refusal("no project " + s)
	}
	return p.ID, nil
}
