package brain

import (
	"cmp"
	"context"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// The product context and the knowledge entries were two stores before the Brain; their doors
// (/projects/{id}/product, /knowledge, gator-cli product propose) stay, over the Brain, for one
// release. Migration 00043 moved their rows here with their ids, and left views under their old
// names for anything that still reads them in SQL.

// ProductKind is the product context's name for a Brain kind: "principles" for principle.
func ProductKind(kind string) string {
	if kind == KindPrinciple {
		return "principles"
	}
	return kind
}

// KindOfProduct is the Brain's kind for a product context kind.
func KindOfProduct(kind string) string {
	if kind == "principles" {
		return KindPrinciple
	}
	return kind
}

// ProductStatus is the product context's status for an entry. It knew three: a version a newer
// one replaced was still approved there (the newest approved one was read), and a proposal
// turned down was archived.
func ProductStatus(status string) string {
	switch status {
	case StatusSuperseded:
		return StatusApproved
	case StatusRejected:
		return StatusArchived
	}
	return status
}

// shelfStore is what the old doors read. *db.Queries is one.
type shelfStore interface {
	ListProductEntries(ctx context.Context, arg db.ListProductEntriesParams) ([]db.BrainEntry, error)
	ListProposedProduct(ctx context.Context, arg db.ListProposedProductParams) ([]db.BrainEntry, error)
	ListKnowledgeShelf(ctx context.Context, projectID pgtype.UUID) ([]db.BrainEntry, error)
	LiveBrainEntries(ctx context.Context, arg db.LiveBrainEntriesParams) ([]db.BrainEntry, error)
	GetBrainEntry(ctx context.Context, id pgtype.UUID) (db.BrainEntry, error)
	DeleteBrainEntry(ctx context.Context, id pgtype.UUID) (int64, error)
}

// Legacy is the product context and the knowledge shelves as they were read and written, over
// the Brain.
type Legacy struct {
	store shelfStore
}

// NewLegacy builds it.
func NewLegacy(store shelfStore) Legacy { return Legacy{store: store} }

// Product is a project's product context, every version.
func (l Legacy) Product(ctx context.Context, project pgtype.UUID) ([]db.BrainEntry, error) {
	return l.store.ListProductEntries(ctx, db.ListProductEntriesParams{ProjectID: project, Kinds: ProductKinds})
}

// ApprovedProduct is what a project decided about its product and still holds: the newest
// approved version of each entry, not folded into a condensed one.
func (l Legacy) ApprovedProduct(ctx context.Context, project pgtype.UUID) ([]db.BrainEntry, error) {
	rows, err := l.store.LiveBrainEntries(ctx, db.LiveBrainEntriesParams{Scopes: []string{ScopeProject}, ProjectID: project})
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, e := range rows {
		if e.ProjectID.Valid && isProductKind(e.Kind) {
			out = append(out, e)
		}
	}
	sortByKindTitle(out)
	return out, nil
}

// ProposedProduct is what waits in the inbox from the product context, in one project or all.
func (l Legacy) ProposedProduct(ctx context.Context, project pgtype.UUID) ([]db.BrainEntry, error) {
	return l.store.ListProposedProduct(ctx, db.ListProposedProductParams{Kinds: ProductKinds, ProjectID: project})
}

// Shelf is the conventions written by hand for a project, or for the workspace (no project).
func (l Legacy) Shelf(ctx context.Context, project pgtype.UUID) ([]db.BrainEntry, error) {
	return l.store.ListKnowledgeShelf(ctx, project)
}

// ShelfEntry is one convention of a shelf.
func (l Legacy) ShelfEntry(ctx context.Context, id pgtype.UUID) (db.BrainEntry, error) {
	e, err := l.store.GetBrainEntry(ctx, id)
	if err != nil {
		return db.BrainEntry{}, notFound(err)
	}
	if e.Kind != KindConvention || e.Topic == nil {
		return db.BrainEntry{}, ErrNotFound
	}
	return e, nil
}

// RemoveFromShelf deletes a convention, as knowledge entries were deleted.
func (l Legacy) RemoveFromShelf(ctx context.Context, id pgtype.UUID) error {
	_, err := l.store.DeleteBrainEntry(ctx, id)
	return err
}

func isProductKind(kind string) bool { return slices.Contains(ProductKinds, kind) }

// sortByKindTitle orders entries as the product context listed them: by its kind, then title.
func sortByKindTitle(es []db.BrainEntry) {
	slices.SortStableFunc(es, func(a, b db.BrainEntry) int {
		return cmp.Or(cmp.Compare(ProductKind(a.Kind), ProductKind(b.Kind)), cmp.Compare(a.Title, b.Title))
	})
}
