package brain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// A proposed decision that is much like an approved one but says something else may replace it:
// it gets a link "contradicts" to that decision, the card in its thread asks "does this replace
// X?", and approving it supersedes X (valid_until now). Nothing is deleted, so "what did we know
// in March" still has an answer (GET /brain?at=).
//
// Similar is decided over what a Searcher finds: full-text search now, or a search that compares
// embeddings (an Embedder) without anything here changing.

// Embedder turns text into vectors, for a Searcher that compares meaning as well as words. None is
// configured by default; full-text search needs none.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Contradictions finds the approved decisions a new one may replace.
type Contradictions struct{ searcher Searcher }

// NewContradictions builds it over a searcher.
func NewContradictions(searcher Searcher) Contradictions { return Contradictions{searcher: searcher} }

// maxCandidates is how many decisions one proposal is compared with.
const maxCandidates = 20

// Of answers the approved decisions of e's project (its component's, or the project's own) that
// share most of e's subject and say something else. Not its own earlier versions: those it
// replaces anyway.
func (c Contradictions) Of(ctx context.Context, e db.BrainEntry) ([]db.BrainEntry, error) {
	if e.Kind != KindDecision || !e.ProjectID.Valid || (e.Scope != ScopeProject && e.Scope != ScopeComponent) {
		return nil, nil
	}
	words := significant(e.Title)
	if len(words) == 0 {
		return nil, nil
	}
	hits, err := c.searcher.Search(ctx, Reach{EveryProject: true}, Query{Text: strings.Join(words, " or "), Project: e.ProjectID,
		Scopes: []string{ScopeProject, ScopeComponent}, Kinds: []string{KindDecision}, Statuses: []string{StatusApproved}, Limit: maxCandidates})
	if err != nil {
		return nil, err
	}
	var out []db.BrainEntry
	for _, h := range hits {
		o := h.Entry
		if o.Lineage == e.Lineage || o.ProjectID != e.ProjectID || !Live(o) || sameText(o.Content, e.Content) {
			continue
		}
		if o.Scope == ScopeComponent && e.Scope == ScopeComponent && deref(o.ComponentKey) != deref(e.ComponentKey) {
			continue // two components may well decide differently
		}
		if alike(e, o) {
			out = append(out, o)
		}
	}
	return out, nil
}

// Link is the Effect of a proposal: it links the decisions it may replace.
func (c Contradictions) Link(ctx context.Context, st Store, e db.BrainEntry) error {
	found, err := c.Of(ctx, e)
	if err != nil {
		return err
	}
	for _, o := range found {
		if err := st.InsertBrainLink(ctx, db.InsertBrainLinkParams{EntryID: e.ID, TargetKind: "entry",
			TargetID: UUIDString(o.ID), Relation: "contradicts"}); err != nil {
			return err
		}
	}
	return nil
}

// SupersedeContradicted is the Effect of approval: what an approved entry contradicts stops being
// true now.
func SupersedeContradicted(ctx context.Context, st Store, e db.BrainEntry) error {
	links, err := st.ListBrainLinks(ctx, []pgtype.UUID{e.ID})
	if err != nil {
		return err
	}
	for _, l := range links {
		if l.Relation != "contradicts" || l.TargetKind != "entry" {
			continue
		}
		id, ok := ParseUUID(l.TargetID)
		if !ok {
			continue
		}
		old, err := st.SupersedeBrainEntry(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // no longer approved: nothing to supersede
		}
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"entry_id": UUIDString(old.ID), "by": UUIDString(e.ID), "kind": old.Kind,
			"task_id": UUIDString(old.SourceTaskID)})
		if _, err := st.InsertEvent(ctx, db.InsertEventParams{Type: "brain.superseded", Aggregate: "project", AggregateID: old.ProjectID, Payload: payload}); err != nil {
			return err
		}
	}
	return nil
}

// alike says two decisions are about the same thing: their titles share two significant words,
// or one and much of what they say.
func alike(a, b db.BrainEntry) bool {
	shared := overlap(significant(a.Title), significant(b.Title))
	if shared >= 2 {
		return true
	}
	if shared == 0 {
		return false
	}
	ca, cb := significant(a.Content), significant(b.Content)
	union := len(ca) + len(cb) - overlap(ca, cb)
	return union > 0 && float64(overlap(ca, cb))/float64(union) >= 0.25
}

// stopwords carry no subject.
var stopwords = map[string]bool{"the": true, "and": true, "for": true, "use": true, "uses": true, "using": true, "with": true,
	"not": true, "our": true, "are": true, "was": true, "this": true, "that": true, "from": true, "into": true, "have": true,
	"will": true, "should": true, "decision": true, "we": true, "all": true, "any": true, "its": true, "now": true}

// significant are the distinct words of s that say what it is about, in order.
func significant(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) < 3 || stopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

func overlap(a, b []string) int {
	in := map[string]bool{}
	for _, w := range a {
		in[w] = true
	}
	n := 0
	for _, w := range b {
		if in[w] {
			n++
		}
	}
	return n
}

func sameText(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
