package brain

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// PluginKinds are what a plugin may propose: what a repository, a tracker or a monitoring tool
// knows (an ADR is a decision, a recurring error a runbook).
var PluginKinds = []string{KindDecision, KindLesson, KindRunbook, KindFact, KindGlossary, KindConvention}

// PluginProposal is what a plugin proposes (brain.propose).
type PluginProposal struct {
	Kind, Title, Content string
	Scope, Component     string
	// SourceID names what it came from in the plugin's own terms; it is proposed once.
	SourceID, URL, Excerpt string
}

// pluginStore is what PluginProposals reads. *db.Queries is one.
type pluginStore interface {
	CountBrainFromSource(ctx context.Context, arg db.CountBrainFromSourceParams) (int64, error)
}

// proposals stores proposals. Service is one.
type proposals interface {
	Propose(ctx context.Context, d Draft, by Author) (db.BrainEntry, error)
}

// PluginProposals is the door plugins propose through: within their own project, always a
// proposal, each source once. What a plugin writes comes from outside the workspace, so it is
// only ever a proposal a person reads before any prompt does.
type PluginProposals struct {
	store pluginStore
	brain proposals
}

// NewPluginProposals builds it.
func NewPluginProposals(store pluginStore, brain proposals) PluginProposals {
	return PluginProposals{store: store, brain: brain}
}

// Propose stores what plugin proposes in project; created is false when that source was proposed
// before.
func (pp PluginProposals) Propose(ctx context.Context, project pgtype.UUID, plugin string, p PluginProposal) (db.BrainEntry, bool, error) {
	p.SourceID = strings.TrimSpace(p.SourceID)
	switch {
	case !slices.Contains(PluginKinds, p.Kind):
		return db.BrainEntry{}, false, invalid("a plugin proposes a %s", strings.Join(PluginKinds, ", "))
	case p.SourceID == "":
		return db.BrainEntry{}, false, invalid("a proposal names its source_id")
	}
	n, err := pp.store.CountBrainFromSource(ctx, db.CountBrainFromSourceParams{SourceKind: SourcePlugin, SourceID: p.SourceID, ProjectID: project})
	if err != nil || n > 0 {
		return db.BrainEntry{}, false, err
	}
	scope := ScopeProject
	if p.Scope == ScopeComponent {
		scope = ScopeComponent
	}
	e, err := pp.brain.Propose(ctx, Draft{Scope: scope, Project: project, Component: p.Component, Kind: p.Kind, Title: p.Title,
		Content: p.Content, Source: Source{Kind: SourcePlugin, Name: plugin, ID: p.SourceID, URL: p.URL, Excerpt: p.Excerpt}},
		Author{Kind: ByPlugin})
	return e, err == nil, err
}
