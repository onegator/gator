package product

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// Thresholds for condensing. A project's product context earns its place in every prompt, so
// the question is not "is this a lot of rows" but "is this more than a reader needs".
const (
	defaultMinEntries = 8
	defaultMinBytes   = 8000
	// maxCondensationInput bounds what one curation task carries. Past this the curator is
	// reading more than it can hold, and a second pass later is better than a truncated one now.
	maxCondensationInput = 40000
)

// curationKind is the task a curator works on.
const curationKind = "curation"

// Condenser opens curation tasks when a project's context has outgrown its prompt, and turns
// the curator's answer into a proposal a person approves. Nothing is folded together without
// that approval, and nothing is ever deleted.
type Condenser struct {
	*Curator
	proc       *process.Service
	MinEntries int
	MinBytes   int
}

// NewCondenser wires one. Zero thresholds take the defaults.
func NewCondenser(c *Curator, proc *process.Service, minEntries, minBytes int) *Condenser {
	if minEntries <= 0 {
		minEntries = defaultMinEntries
	}
	if minBytes <= 0 {
		minBytes = defaultMinBytes
	}
	cd := &Condenser{Curator: c, proc: proc, MinEntries: minEntries, MinBytes: minBytes}
	c.onCurationClosed = cd.proposeCondensate
	return cd
}

// Sweep opens a curation task for every project and kind that has grown too large. Returns how
// many it opened.
func (c *Condenser) Sweep(ctx context.Context) (int, error) {
	q := db.New(c.pool)
	rows, err := q.ProductKindsWorthCondensing(ctx, db.ProductKindsWorthCondensingParams{
		MinEntries: int64(c.MinEntries), MinBytes: int64(c.MinBytes)})
	if err != nil {
		return 0, err
	}
	var opened int
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		started, err := c.open(ctx, row.ProjectID, row.Kind, row.Entries)
		if err != nil {
			c.log.Warn("opening a curation task", "project", uuidString(row.ProjectID), "kind", row.Kind, "err", err)
			continue
		}
		if started {
			opened++
		}
	}
	return opened, nil
}

// open creates the curation task for one kind, unless one is already open for it.
func (c *Condenser) open(ctx context.Context, projectID pgtype.UUID, kind string, entries int64) (bool, error) {
	q := db.New(c.pool)
	title := curationTitle(kind)
	// A slow agent must not collect a queue of identical tasks; one open curation per kind is
	// the whole point of asking.
	n, err := q.OpenCondensationTask(ctx, db.OpenCondensationTaskParams{ProjectID: projectID, Title: title})
	if err != nil || n > 0 {
		return false, err
	}
	live, err := q.LiveProductEntriesOfKind(ctx, db.LiveProductEntriesOfKindParams{ProjectID: projectID, Kind: kind})
	if err != nil {
		return false, err
	}
	if len(live) == 0 {
		return false, nil
	}
	if _, err := c.proc.Create(ctx, process.CreateParams{
		ProjectID: projectID, Kind: curationKind, Title: title,
		Description: condensationBrief(kind, live, entries), Urgency: 4,
	}, process.Actor{Kind: process.ActorSystem}); err != nil {
		return false, err
	}
	return true, nil
}

func curationTitle(kind string) string {
	return "Condense the " + kind + " entries"
}

// condensationBrief is what the curator reads: every entry, whole, with the instruction. The
// entries are quoted rather than summarised here — a summary of a summary is how the detail
// that mattered disappears.
func condensationBrief(kind string, entries []db.ProductContext, count int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This project has %d %s entries in its product context, which is more than a prompt should carry.\n\n", count, kind)
	b.WriteString("Propose one entry that replaces them. The originals are kept and archived once somebody approves your proposal, so write the entry you would want read from now on.\n\n")
	b.WriteString("---\n\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "## %s (v%d)\n\n%s\n\n", e.Title, e.Version, strings.TrimSpace(e.Content))
		if b.Len() > maxCondensationInput {
			b.WriteString("…(the rest is left for a later pass)\n")
			break
		}
	}
	return b.String()
}

// proposeCondensate turns a finished curation task into a proposal of the kind it was about,
// naming the entries it was made from so approving it archives exactly those.
func (c *Condenser) proposeCondensate(ctx context.Context, task db.Task) error {
	kind := kindFromCurationTitle(task.Title)
	if kind == "" {
		return nil
	}
	q := db.New(c.pool)
	n, err := q.CountProposalsForTask(ctx, db.CountProposalsForTaskParams{
		ProjectID: task.ProjectID, Kind: kind, SourceTaskID: task.ID})
	if err != nil || n > 0 {
		return err
	}
	artifacts, err := q.ListArtifacts(ctx, task.ID)
	if err != nil {
		return err
	}
	var report db.Artifact
	for _, a := range artifacts {
		if a.Content != nil && a.ApprovedAt.Valid {
			report = a
		}
	}
	if report.Content == nil {
		return nil // the curator has not answered yet
	}
	live, err := q.LiveProductEntriesOfKind(ctx, db.LiveProductEntriesOfKindParams{ProjectID: task.ProjectID, Kind: kind})
	if err != nil {
		return err
	}
	ids := make([]pgtype.UUID, 0, len(live))
	for _, e := range live {
		ids = append(ids, e.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	entry, err := q.CreateCondensedEntry(ctx, db.CreateCondensedEntryParams{
		ProjectID: task.ProjectID, Kind: kind, Title: curationTitle(kind),
		Content: strings.TrimSpace(*report.Content), SourceTaskID: task.ID, CondensedFrom: ids})
	if err != nil {
		return err
	}
	c.log.Info("proposed a condensed product entry", "project", uuidString(task.ProjectID),
		"kind", kind, "replaces", len(ids), "entry", uuidString(entry.ID))
	return nil
}

// kindFromCurationTitle reads the kind back out of the task's title, which is how a curation
// task says what it is about without a column of its own.
func kindFromCurationTitle(title string) string {
	const prefix, suffix = "Condense the ", " entries"
	if !strings.HasPrefix(title, prefix) || !strings.HasSuffix(title, suffix) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(title, prefix), suffix)
}

// ArchiveCondensedSources is called when a condensed entry is approved: the entries it replaces
// are archived, not deleted. They stay readable for anyone who wants to know what was folded
// together, and they stop being carried into every prompt.
func ArchiveCondensedSources(ctx context.Context, q *db.Queries, entry db.ProductContext) (int64, error) {
	if len(entry.CondensedFrom) == 0 || entry.Status != "approved" {
		return 0, nil
	}
	return q.ArchiveProductEntries(ctx, entry.CondensedFrom)
}
