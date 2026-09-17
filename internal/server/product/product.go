// Package product keeps what a project knows about its own product: the owner's vision and
// principles, and the decisions and lessons the work leaves behind. It proposes entries from
// finished work; a person approves them, and only approved entries reach a job's prompt.
package product

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/store/db"
)

// cursorName is this consumer's row in event_cursors.
const cursorName = "product"

// maxProposal bounds what a proposal carries from a document.
const maxProposal = 1500

// Curator turns finished work into proposals a person can approve.
type Curator struct {
	pool *pgxpool.Pool
	hub  *events.Hub
	log  *slog.Logger
	poll time.Duration
}

// NewCurator builds the consumer; poll defaults to five seconds.
func NewCurator(pool *pgxpool.Pool, hub *events.Hub, log *slog.Logger, poll time.Duration) *Curator {
	if poll == 0 {
		poll = 5 * time.Second
	}
	return &Curator{pool: pool, hub: hub, log: log, poll: poll}
}

// watched are the events that leave something worth remembering.
var watched = []string{"gate.approved", "task.closed"}

// Run proposes entries until ctx ends.
func (c *Curator) Run(ctx context.Context) {
	wake, unsub := c.hub.Subscribe("*")
	defer unsub()
	if err := db.New(c.pool).InitEventCursor(ctx, cursorName); err != nil && ctx.Err() == nil {
		c.log.Warn("product cursor", "err", err)
	}
	t := time.NewTicker(c.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			c.drain(ctx)
		case <-t.C:
			c.drain(ctx)
		}
	}
}

func (c *Curator) drain(ctx context.Context) {
	q := db.New(c.pool)
	for ctx.Err() == nil {
		cursor, err := q.GetEventCursor(ctx, cursorName)
		if err != nil {
			return
		}
		rows, err := q.EventsAfter(ctx, db.EventsAfterParams{After: cursor, Types: watched, MaxRows: 100})
		if err != nil || len(rows) == 0 {
			return
		}
		for _, r := range rows {
			if err := c.handle(ctx, r); err != nil && ctx.Err() == nil {
				c.log.Warn("product proposal", "event", r.Type, "err", err)
			}
		}
		if err := q.SetEventCursor(ctx, db.SetEventCursorParams{Name: cursorName, EventID: rows[len(rows)-1].ID}); err != nil {
			return
		}
	}
}

func (c *Curator) handle(ctx context.Context, row db.EventsAfterRow) error {
	if !row.AggregateID.Valid {
		return nil
	}
	q := db.New(c.pool)
	task, err := q.GetTask(ctx, row.AggregateID)
	if err != nil {
		return err
	}
	switch row.Type {
	case "gate.approved":
		var payload struct {
			Phase string `json:"phase"`
		}
		_ = json.Unmarshal(row.Payload, &payload)
		return c.proposeDecision(ctx, task, payload.Phase)
	case "task.closed":
		return c.proposeLesson(ctx, task)
	}
	return nil
}

// proposeDecision records what a project decided when its owner approved a brief or a plan.
func (c *Curator) proposeDecision(ctx context.Context, task db.Task, phase string) error {
	if phase != "discovery" && phase != "planning" {
		return nil // other gates approve work, not direction
	}
	q := db.New(c.pool)
	n, err := q.CountProposalsForTask(ctx, db.CountProposalsForTaskParams{ProjectID: task.ProjectID, Kind: "decision", SourceTaskID: task.ID})
	if err != nil || n > 0 {
		return err
	}
	artifacts, err := q.ListArtifacts(ctx, task.ID)
	if err != nil {
		return err
	}
	var document db.Artifact
	for _, a := range artifacts {
		if a.Phase == phase && a.Content != nil {
			document = a
		}
	}
	if document.Content == nil {
		return nil
	}
	body := fmt.Sprintf("Approved in %s of “%s”.\n\n%s", phase, task.Title, clip(*document.Content, maxProposal))
	return c.propose(ctx, task, "decision", task.Title, body)
}

// proposeLesson records why a task had to go back, so the next one does not repeat it.
func (c *Curator) proposeLesson(ctx context.Context, task db.Task) error {
	q := db.New(c.pool)
	transitions, err := q.ListPhaseTransitions(ctx, task.ID)
	if err != nil {
		return err
	}
	var reasons []string
	for _, t := range transitions {
		if t.Kind == "rollback" && t.Reason != nil && *t.Reason != "" {
			reasons = append(reasons, fmt.Sprintf("- back to %s: %s", t.ToPhase, *t.Reason))
		}
	}
	if len(reasons) == 0 {
		return nil // nothing went wrong; nothing to learn
	}
	n, err := q.CountProposalsForTask(ctx, db.CountProposalsForTaskParams{ProjectID: task.ProjectID, Kind: "lesson", SourceTaskID: task.ID})
	if err != nil || n > 0 {
		return err
	}
	body := fmt.Sprintf("“%s” was sent back %d time(s):\n\n%s", task.Title, len(reasons), strings.Join(reasons, "\n"))
	return c.propose(ctx, task, "lesson", task.Title, clip(body, maxProposal))
}

func (c *Curator) propose(ctx context.Context, task db.Task, kind, title, content string) error {
	q := db.New(c.pool)
	entry, err := q.CreateProductEntry(ctx, db.CreateProductEntryParams{
		ProjectID: task.ProjectID, Kind: kind, Title: title, Content: content, Status: "proposed",
		SourceTaskID: task.ID, CreatedByKind: "system",
	})
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{"kind": kind, "title": title, "entry_id": uuidString(entry.ID)})
	_, err = q.InsertEvent(ctx, db.InsertEventParams{Type: "product.proposed", Aggregate: "project", AggregateID: task.ProjectID, Payload: b})
	return err
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n\n…"
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
