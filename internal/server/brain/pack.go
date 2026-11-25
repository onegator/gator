package brain

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// The package a job is handed from the Brain: deterministic, like the product slice was — the
// same entries in the same order for the same job, within a budget of bytes — and read under the
// same Policy as every other door, so a turn carries the private context of the person who asked
// for it and of nobody else.

// Budgets of a package, in bytes of text.
const (
	// DefaultPackBytes is how much of the Brain one job carries.
	DefaultPackBytes = 24000
	// maxItemBytes bounds one entry, so one long runbook does not crowd out the rest.
	maxItemBytes = 6000
)

// Reasons an entry did not arrive whole, read by people.
const (
	LeftTooLong = "too long on its own; the rest was cut"
	LeftNoRoom  = "no room left in the package"
)

// Request is what a package is made for.
type Request struct {
	// Reader is the job's agent as the rules see it (AgentReader).
	Reader Reader
	// Components are the task's component first, then its neighbours, by key.
	Components []string
	// Agent is the job's agent, whose own lessons it carries.
	Agent pgtype.UUID
	// Product says the job's role weighs direction, and so reads the product context.
	Product bool
	// Budget in bytes; 0 is DefaultPackBytes.
	Budget int
}

// Item is one entry as a package carries it.
type Item struct {
	Entry db.BrainEntry
	Kind  string // "product" for the product context, "brain" for the rest
	Phase string
	Title string
	Body  string
	// FullBytes is the entry's length before any cut; Left says why it was cut or left out, and
	// Dropped that it never reached the prompt.
	FullBytes int
	Left      string
	Dropped   bool
	// Private is a person's own context: handed to the agent, never to the job's record.
	Private bool
}

// A section is one part of a package: which live entries it takes, and how it names them. The
// order of sections is the order a job reads them; a new kind of context is one more section.
type section struct {
	takes func(e db.BrainEntry, req Request) bool
	label func(e db.BrainEntry) (kind, phase, title string)
}

// sections are the parts of a package. The conventions on the knowledge shelves (with a topic)
// are not among them: they reach a job with the knowledge packs they can shadow.
var sections = []section{
	{ // what the task's part of the product is like, and its neighbours'
		takes: func(e db.BrainEntry, req Request) bool {
			return e.Scope == ScopeComponent && e.ComponentKey != nil && slices.Contains(req.Components, *e.ComponentKey)
		},
		label: func(e db.BrainEntry) (string, string, string) {
			return "brain", ScopeComponent, fmt.Sprintf("%s %s: %s", *e.ComponentKey, e.Kind, e.Title)
		},
	},
	{ // what the product decided about itself, for the roles that weigh direction
		takes: func(e db.BrainEntry, req Request) bool {
			return req.Product && e.Scope == ScopeProject && slices.Contains(ProductKinds, e.Kind)
		},
		label: func(e db.BrainEntry) (string, string, string) {
			return "product", ProductKind(e.Kind), fmt.Sprintf("%s: %s", ProductKind(e.Kind), e.Title)
		},
	},
	{ // how this project runs: runbooks, facts, its own conventions
		takes: func(e db.BrainEntry, _ Request) bool {
			return e.Scope == ScopeProject && !slices.Contains(ProductKinds, e.Kind) && e.Topic == nil
		},
		label: func(e db.BrainEntry) (string, string, string) {
			return "brain", ScopeProject, fmt.Sprintf("%s: %s", e.Kind, e.Title)
		},
	},
	{ // what this agent learned here
		takes: func(e db.BrainEntry, req Request) bool {
			return e.Scope == ScopeAgent && req.Agent.Valid && e.AgentID.Bytes == req.Agent.Bytes
		},
		label: func(e db.BrainEntry) (string, string, string) {
			return "brain", ScopeAgent, fmt.Sprintf("your %s: %s", e.Kind, e.Title)
		},
	},
	{ // the people: what they share, and the context of the one who asked
		takes: func(e db.BrainEntry, _ Request) bool { return e.Scope == ScopeUser },
		label: func(e db.BrainEntry) (string, string, string) {
			return "brain", ScopeUser, fmt.Sprintf("a person's %s: %s", e.Kind, e.Title)
		},
	},
	{ // the workspace's own, beyond its knowledge shelves
		takes: func(e db.BrainEntry, _ Request) bool { return e.Scope == ScopeWorkspace && e.Topic == nil },
		label: func(e db.BrainEntry) (string, string, string) {
			return "brain", ScopeWorkspace, fmt.Sprintf("%s: %s", e.Kind, e.Title)
		},
	},
}

// packStore is what the packer reads. *db.Queries is one.
type packStore interface {
	LiveBrainEntries(ctx context.Context, arg db.LiveBrainEntriesParams) ([]db.BrainEntry, error)
}

// Packer makes the package. It reads; searching is the Finder's and recording what was handed
// over is the caller's (Citations).
type Packer struct {
	store  packStore
	policy Policy
}

// NewPacker builds it.
func NewPacker(store packStore, policy Policy) Packer { return Packer{store: store, policy: policy} }

// Pack answers the items of req's package, in reading order, each within the budget or marked
// as left out.
func (p Packer) Pack(ctx context.Context, req Request) ([]Item, error) {
	if !req.Reader.IsJob() {
		return nil, ErrForbidden // a package is an agent's
	}
	live, err := p.store.LiveBrainEntries(ctx, db.LiveBrainEntriesParams{Scopes: Scopes, ProjectID: req.Reader.Project})
	if err != nil {
		return nil, err
	}
	readable, err := p.readable(ctx, req.Reader, live)
	if err != nil {
		return nil, err
	}
	budget := cmp.Or(req.Budget, DefaultPackBytes)
	var out []Item
	used := 0
	for _, sec := range sections {
		var taken []db.BrainEntry
		for _, e := range readable {
			if sec.takes(e, req) {
				taken = append(taken, e)
			}
		}
		order(taken, req.Components)
		for _, e := range taken {
			item := render(sec, e, req.Reader)
			used = fit(&item, used, budget)
			out = append(out, item)
		}
	}
	return out, nil
}

func (p Packer) readable(ctx context.Context, r Reader, es []db.BrainEntry) ([]db.BrainEntry, error) {
	out := es[:0]
	for _, e := range es {
		ok, err := p.policy.CanRead(ctx, r, e)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// order is a section's reading order: the task's own component before its neighbours, then by
// kind, title and id, so the same job gets the same package.
func order(es []db.BrainEntry, components []string) {
	rank := func(e db.BrainEntry) int {
		if e.ComponentKey == nil {
			return 0
		}
		return slices.Index(components, *e.ComponentKey)
	}
	slices.SortStableFunc(es, func(a, b db.BrainEntry) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(ProductKind(a.Kind), ProductKind(b.Kind)),
			cmp.Compare(a.Title, b.Title), cmp.Compare(UUIDString(a.ID), UUIDString(b.ID)))
	})
}

func render(sec section, e db.BrainEntry, r Reader) Item {
	kind, phase, title := sec.label(e)
	return Item{Entry: e, Kind: kind, Phase: phase, Title: fmt.Sprintf("%s (v%d) %s", title, e.Version, Tag(e.ID)),
		Body: e.Content, FullBytes: len(e.Content), Private: e.Scope == ScopeUser && !sharedWith(e)}
}

// sharedWith says whether a person's entry is open beyond them.
func sharedWith(e db.BrainEntry) bool {
	return e.Visibility == VisibilityProject || e.Visibility == VisibilityWorkspace
}

// fit cuts item to what is left of the budget and answers how much is used after it.
func fit(item *Item, used, budget int) int {
	if used >= budget {
		item.Body, item.Dropped, item.Left = "", true, LeftNoRoom
		return used
	}
	if len(item.Body) > maxItemBytes {
		item.Body, item.Left = clip(item.Body, maxItemBytes)+"\n… [truncated]", LeftTooLong
	}
	if used+len(item.Body) > budget {
		item.Body, item.Left = clip(item.Body, budget-used)+"\n… [truncated]", LeftNoRoom
	}
	return used + len(item.Body)
}

// Tag is how a package names an entry, and how an agent cites it back.
func Tag(id pgtype.UUID) string { return "[brain:" + UUIDString(id) + "]" }

var tagPattern = regexp.MustCompile(`\[brain:([0-9a-fA-F-]{36})\]`)

// Tags are the entries a text cites, each once, in order.
func Tags(text string) []pgtype.UUID {
	var out []pgtype.UUID
	seen := map[[16]byte]bool{}
	for _, m := range tagPattern.FindAllStringSubmatch(text, -1) {
		if id, ok := ParseUUID(m[1]); ok && !seen[id.Bytes] {
			seen[id.Bytes] = true
			out = append(out, id)
		}
	}
	return out
}

// CiteNote is what a package says about citing, once, before its first entry.
const CiteNote = "Entries from the Brain carry a tag like [brain:<id>] in their title. When your work or your answer " +
	"relies on one, write its tag in your final message: that is how a person sees where you got it from."
