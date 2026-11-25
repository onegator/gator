package brain

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

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

// DefaultPluginPerDay is how much one plugin proposes in one project a day.
const DefaultPluginPerDay = 50

// pluginStore is what PluginProposals reads. *db.Queries is one.
type pluginStore interface {
	CountPluginSource(ctx context.Context, arg db.CountPluginSourceParams) (int64, error)
	CountPluginProposalsSince(ctx context.Context, arg db.CountPluginProposalsSinceParams) (int64, error)
}

// proposals stores proposals. Service is one.
type proposals interface {
	Propose(ctx context.Context, d Draft, by Author) (db.BrainEntry, error)
}

// PluginProposals is the door plugins propose through: within their own project, always a
// proposal, each source once. What a plugin writes comes from outside the workspace, so it is
// only ever a proposal a person reads before any prompt does — and, approved, still reaches one
// fenced as outside words (see the Packer).
type PluginProposals struct {
	store pluginStore
	brain proposals
	// PerDay bounds what one plugin proposes in one project in a day.
	PerDay int
}

// NewPluginProposals builds it.
func NewPluginProposals(store pluginStore, brain proposals) *PluginProposals {
	return &PluginProposals{store: store, brain: brain, PerDay: DefaultPluginPerDay}
}

// Propose stores what plugin proposes in project; created is false when that source was proposed
// before.
func (pp *PluginProposals) Propose(ctx context.Context, project pgtype.UUID, plugin string, p PluginProposal) (db.BrainEntry, bool, error) {
	p.SourceID = strings.TrimSpace(p.SourceID)
	switch {
	case !slices.Contains(PluginKinds, p.Kind):
		return db.BrainEntry{}, false, invalid("a plugin proposes a %s", strings.Join(PluginKinds, ", "))
	case p.SourceID == "":
		return db.BrainEntry{}, false, invalid("a proposal names its source_id")
	}
	n, err := pp.store.CountPluginSource(ctx, db.CountPluginSourceParams{Plugin: plugin, SourceID: p.SourceID, ProjectID: project})
	if err != nil || n > 0 {
		return db.BrainEntry{}, false, err
	}
	today, err := pp.store.CountPluginProposalsSince(ctx, db.CountPluginProposalsSinceParams{Plugin: plugin, ProjectID: project,
		Since: pgtype.Timestamptz{Time: now().Add(-24 * time.Hour), Valid: true}})
	if err != nil {
		return db.BrainEntry{}, false, err
	}
	if today >= int64(pp.PerDay) {
		return db.BrainEntry{}, false, invalid("%s has proposed %d entries in this project today, which is its limit; try tomorrow", plugin, pp.PerDay)
	}
	scope := ScopeProject
	if p.Scope == ScopeComponent {
		scope = ScopeComponent
	}
	e, err := pp.brain.Propose(ctx, Draft{Scope: scope, Project: project, Component: p.Component, Kind: p.Kind, Title: p.Title,
		Content: p.Content, Source: Source{Kind: SourcePlugin, Name: plugin, ID: p.SourceID, URL: p.URL, Excerpt: p.Excerpt}},
		Author{Kind: ByPlugin})
	if errors.Is(err, ErrConflict) {
		return db.BrainEntry{}, false, nil // the same source, proposed by a delivery at the same moment
	}
	return e, err == nil, err
}
