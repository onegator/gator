package quality

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Whether the Brain helps is measured, not declared: a job should not be handed much more to read
// (the median context of a phase job grows by at most MaxContextGrowth) and fewer phases should be
// sent back. Two windows of the same length either side of a moment — the day the Brain was
// switched on, or any other — are compared.

// MaxContextGrowth is how much more a median job may carry before the Brain costs more than it gives.
const MaxContextGrowth = 0.2

// Sample is one window of a project's work.
type Sample struct {
	Jobs               int
	MedianContextBytes int
	Advances           int
	Rollbacks          int
	// RollbackRate is the share of phase moves that went back.
	RollbackRate float64
}

// Impact compares a window before a moment with one after it.
type Impact struct {
	Before, After Sample
	// ContextGrowth is how much the median context grew, as a fraction of what it was.
	ContextGrowth float64
	// ContextHeld says it grew by at most MaxContextGrowth; RollbacksFell that fewer phases went
	// back. A window with no work in it says neither.
	ContextHeld   bool
	RollbacksFell bool
}

// Median is the middle value of xs (the lower of the two middles for an even count); 0 for none.
func Median(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[(len(s)-1)/2]
}

// NewSample is a window's numbers.
func NewSample(contextBytes []int, advances, rollbacks int) Sample {
	s := Sample{Jobs: len(contextBytes), MedianContextBytes: Median(contextBytes), Advances: advances, Rollbacks: rollbacks}
	if moves := advances + rollbacks; moves > 0 {
		s.RollbackRate = float64(rollbacks) / float64(moves)
	}
	return s
}

// Compare says what changed from before to after.
func Compare(before, after Sample) Impact {
	out := Impact{Before: before, After: after}
	if before.MedianContextBytes > 0 {
		out.ContextGrowth = float64(after.MedianContextBytes-before.MedianContextBytes) / float64(before.MedianContextBytes)
		out.ContextHeld = after.Jobs > 0 && out.ContextGrowth <= MaxContextGrowth
	}
	out.RollbacksFell = before.Advances+before.Rollbacks > 0 && after.Advances+after.Rollbacks > 0 &&
		after.RollbackRate < before.RollbackRate
	return out
}

// BrainImpact compares a project's window before pivot with the one after it, each window long.
func (s *Service) BrainImpact(ctx context.Context, project pgtype.UUID, pivot time.Time, window time.Duration) (Impact, error) {
	before, err := s.sample(ctx, project, pivot.Add(-window), pivot)
	if err != nil {
		return Impact{}, err
	}
	after, err := s.sample(ctx, project, pivot, pivot.Add(window))
	if err != nil {
		return Impact{}, err
	}
	return Compare(before, after), nil
}

func (s *Service) sample(ctx context.Context, project pgtype.UUID, since, until time.Time) (Sample, error) {
	q := db.New(s.pool)
	window := func(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
	bytes, err := q.PhaseJobContextBytes(ctx, db.PhaseJobContextBytesParams{ProjectID: project, Since: window(since), Until: window(until)})
	if err != nil {
		return Sample{}, err
	}
	moves, err := q.PhaseMoves(ctx, db.PhaseMovesParams{ProjectID: project, Since: window(since), Until: window(until)})
	if err != nil {
		return Sample{}, err
	}
	sizes := make([]int, 0, len(bytes))
	for _, b := range bytes {
		sizes = append(sizes, int(b))
	}
	return NewSample(sizes, int(moves.Advances), int(moves.Rollbacks)), nil
}
