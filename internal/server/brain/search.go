package brain

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Query is what a reader looks for.
type Query struct {
	Text     string
	Scopes   []string
	Kinds    []string
	Statuses []string
	// Project narrows to one project and what is above it (the workspace, people's shared context).
	Project pgtype.UUID
	// Archived includes entries folded into a condensed one.
	Archived bool
	Limit    int
}

// Hit is one entry found, with how well it matched.
type Hit struct {
	Entry db.BrainEntry
	Rank  float32
}

// Searcher finds candidate entries within a reach. Full-text search in Postgres is one (FTS); one
// that also compares embeddings can stand in for it without anything above it changing.
type Searcher interface {
	Search(ctx context.Context, reach Reach, q Query) ([]Hit, error)
}

// ftsStore is what FTS reads. *db.Queries is one.
type ftsStore interface {
	SearchBrain(ctx context.Context, arg db.SearchBrainParams) ([]db.SearchBrainRow, error)
}

// FTS searches by words with Postgres full-text search ('simple' configuration, websearch syntax),
// ranked with ts_rank; without words it lists the newest first.
type FTS struct{ store ftsStore }

// NewFTS builds it.
func NewFTS(store ftsStore) FTS { return FTS{store: store} }

// Search implements Searcher.
func (f FTS) Search(ctx context.Context, reach Reach, q Query) ([]Hit, error) {
	statuses := q.Statuses
	if reach.LiveOnly {
		statuses = []string{StatusApproved}
	}
	rows, err := f.store.SearchBrain(ctx, db.SearchBrainParams{
		Query: strings.TrimSpace(q.Text), Scopes: orEmpty(q.Scopes), Kinds: orEmpty(q.Kinds), Statuses: orEmpty(statuses),
		ProjectID: q.Project, IncludeArchived: q.Archived && !reach.LiveOnly,
		EveryProject: reach.EveryProject, Projects: orNone(reach.Projects), UserID: reach.User, MaxRows: int32(q.Limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(rows))
	for _, r := range rows {
		out = append(out, Hit{Entry: r.BrainEntry, Rank: r.Rank})
	}
	return out, nil
}

// Finder answers a reader's search: the searcher finds candidates, the policy keeps what the
// reader may read. The REST API and the MCP server both ask it, so they answer the same.
type Finder struct {
	searcher Searcher
	policy   Policy
}

// NewFinder builds it.
func NewFinder(searcher Searcher, policy Policy) Finder {
	return Finder{searcher: searcher, policy: policy}
}

// Limits of one search.
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// Find answers q for r, best first.
func (f Finder) Find(ctx context.Context, r Reader, q Query) ([]Hit, error) {
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	q.Limit = min(q.Limit, MaxLimit)
	want := q.Limit
	// The policy may turn candidates away (a private conversation's proposal), so ask for more
	// than are wanted, once.
	q.Limit = want * 3
	hits, err := f.searcher.Search(ctx, ReachOf(r), q)
	if err != nil {
		return nil, err
	}
	out := make([]Hit, 0, min(len(hits), want))
	for _, h := range hits {
		ok, err := f.policy.CanRead(ctx, r, h.Entry)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, h)
		}
		if len(out) == want {
			break
		}
	}
	return out, nil
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orNone(s []pgtype.UUID) []pgtype.UUID {
	if s == nil {
		return []pgtype.UUID{}
	}
	return s
}

// Summary is one hit as a reader is shown it, the same through every door (the REST API's
// GET /brain and the MCP server's brain_search).
type Summary struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Excerpt      string  `json:"excerpt"`
	Scope        string  `json:"scope"`
	Kind         string  `json:"kind"`
	Status       string  `json:"status"`
	Version      int     `json:"version"`
	ProjectID    string  `json:"projectId,omitempty"`
	ComponentKey string  `json:"componentKey,omitempty"`
	Source       Source  `json:"source"`
	Rank         float32 `json:"rank"`
}

// excerptBytes is how much of an entry a hit quotes.
const excerptBytes = 280

// Summarize is h as a reader is shown it.
func Summarize(h Hit) Summary {
	e := h.Entry
	s := Summary{ID: UUIDString(e.ID), Title: e.Title, Excerpt: clip(e.Content, excerptBytes), Scope: e.Scope, Kind: e.Kind,
		Status: e.Status, Version: int(e.Version), ProjectID: UUIDString(e.ProjectID), Source: SourceOf(e), Rank: h.Rank}
	if e.ComponentKey != nil {
		s.ComponentKey = *e.ComponentKey
	}
	return s
}
