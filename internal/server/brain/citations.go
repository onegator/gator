package brain

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Ways a job used an entry.
const (
	HowPacked   = "packed"   // it was in the job's package
	HowSearched = "searched" // the agent looked it up
	HowCited    = "cited"    // the agent said it relied on it
)

// citeStore is what Citations writes. *db.Queries is one.
type citeStore interface {
	RecordBrainCitations(ctx context.Context, arg db.RecordBrainCitationsParams) error
	RecordCitedBrain(ctx context.Context, arg db.RecordCitedBrainParams) error
}

// Citations records which entries a job was handed, looked up and cited: what "how does it know
// that" is answered from, and what shows an entry nobody uses any more.
type Citations struct{ store citeStore }

// NewCitations builds it.
func NewCitations(store citeStore) Citations { return Citations{store: store} }

// Used records that job used entries in one way.
func (c Citations) Used(ctx context.Context, job pgtype.UUID, how string, entries []pgtype.UUID) error {
	if len(entries) == 0 || !job.Valid {
		return nil
	}
	return c.store.RecordBrainCitations(ctx, db.RecordBrainCitationsParams{JobID: job, EntryIds: entries, How: how})
}

// Cited records the entries job's final words cite ([brain:<id>]), of those it was handed or
// looked up.
func (c Citations) Cited(ctx context.Context, job pgtype.UUID, text string) error {
	ids := Tags(text)
	if len(ids) == 0 || !job.Valid {
		return nil
	}
	return c.store.RecordCitedBrain(ctx, db.RecordCitedBrainParams{JobID: job, EntryIds: ids})
}

// IDs are the entries of hits.
func IDs(hits []Hit) []pgtype.UUID {
	out := make([]pgtype.UUID, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Entry.ID)
	}
	return out
}
