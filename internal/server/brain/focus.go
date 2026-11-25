package brain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// A person's focus is worked out, not written: the open tasks they own and the ones whose thread
// they talked in this week, per project. It is the one entry that needs nobody's approval
// (confidence inferred), because it says only what the person's own work says, and it is theirs
// alone: an agent reads it only in a turn that person asked for.

// SourceActivity is where a worked-out entry comes from: the person's own work.
const SourceActivity = "activity"

// focusWindow is how far back talking about a task counts.
const focusWindow = 7 * 24 * time.Hour

// focusStore is what Focus reads. *db.Queries is one.
type focusStore interface {
	ActiveTasksOf(ctx context.Context, arg db.ActiveTasksOfParams) ([]db.ActiveTasksOfRow, error)
	PeopleActiveSince(ctx context.Context, since pgtype.Timestamptz) ([]pgtype.UUID, error)
	CurrentFocusOf(ctx context.Context, userID pgtype.UUID) ([]db.BrainEntry, error)
	FocusOffSince(ctx context.Context, userID pgtype.UUID) (pgtype.Timestamptz, error)
}

// recorder stores what Gator knows itself. Service is one.
type recorder interface {
	Record(ctx context.Context, d Draft, by Author, status string) (db.BrainEntry, error)
	Decide(ctx context.Context, r Reader, id pgtype.UUID, verdict string) (db.BrainEntry, error)
}

// Focus keeps people's focus up to date.
type Focus struct {
	store focusStore
	brain recorder
	log   *slog.Logger
	now   func() time.Time
}

// NewFocus builds it.
func NewFocus(store focusStore, brain recorder, log *slog.Logger) *Focus {
	if log == nil {
		log = slog.Default()
	}
	return &Focus{store: store, brain: brain, log: log, now: time.Now}
}

// Run refreshes the focus of everyone who worked or talked lately, every so often, until ctx ends.
func (f *Focus) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := f.Sweep(ctx); err != nil && ctx.Err() == nil {
				f.log.Warn("working out people's focus", "err", err)
			}
		}
	}
}

// Sweep refreshes the focus of everyone active since the last window.
func (f *Focus) Sweep(ctx context.Context) error {
	people, err := f.store.PeopleActiveSince(ctx, pgtype.Timestamptz{Time: f.now().Add(-focusWindow), Valid: true})
	if err != nil {
		return err
	}
	for _, u := range people {
		if err := f.Refresh(ctx, u); err != nil {
			f.log.Warn("working out a person's focus", "user", UUIDString(u), "err", err)
		}
	}
	return nil
}

// Refresh works out user's focus in every project they work in. A focus that says what it said is
// left alone; one that changed is a new version; one about a project they no longer work in is
// archived.
func (f *Focus) Refresh(ctx context.Context, user pgtype.UUID) error {
	// Somebody who asked the Brain to forget them, or turned their focus off, is left alone.
	if _, err := f.store.FocusOffSince(ctx, user); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	tasks, err := f.store.ActiveTasksOf(ctx, db.ActiveTasksOfParams{UserID: user, Since: pgtype.Timestamptz{Time: f.now().Add(-focusWindow), Valid: true}})
	if err != nil {
		return err
	}
	byProject := map[[16]byte][]db.ActiveTasksOfRow{}
	var order []pgtype.UUID
	for _, t := range tasks {
		if _, seen := byProject[t.ProjectID.Bytes]; !seen {
			order = append(order, t.ProjectID)
		}
		byProject[t.ProjectID.Bytes] = append(byProject[t.ProjectID.Bytes], t)
	}
	current, err := f.store.CurrentFocusOf(ctx, user)
	if err != nil {
		return err
	}
	had := map[[16]byte]db.BrainEntry{}
	for _, e := range current {
		had[e.ProjectID.Bytes] = e
	}
	for _, project := range order {
		content := focusText(byProject[project.Bytes])
		if prev, ok := had[project.Bytes]; ok && prev.Content == content {
			delete(had, project.Bytes)
			continue
		}
		delete(had, project.Bytes)
		if _, err := f.brain.Record(ctx, Draft{Scope: ScopeUser, User: user, Project: project, Kind: KindFocus, Title: "Focus",
			Content: content, Visibility: VisibilityPrivate, Confidence: ConfidenceInferred, Source: Source{Kind: SourceActivity}},
			Author{Kind: BySystem}, StatusApproved); err != nil {
			return err
		}
	}
	for _, stale := range had {
		if _, err := f.brain.Decide(ctx, Reader{User: user}, stale.ID, VerdictArchive); err != nil {
			return err
		}
	}
	return nil
}

func focusText(tasks []db.ActiveTasksOfRow) string {
	var own, talked []string
	for _, t := range tasks {
		line := fmt.Sprintf("- %s (%s)", t.Title, t.Phase)
		if t.Owns {
			own = append(own, line)
		} else {
			talked = append(talked, line)
		}
	}
	var b strings.Builder
	if len(own) > 0 {
		b.WriteString("Owns:\n" + strings.Join(own, "\n"))
	}
	if len(talked) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("Talked about this week:\n" + strings.Join(talked, "\n"))
	}
	return b.String()
}
