// Package notify turns domain events into push notifications. It reads the outbox with its
// own durable cursor, decides who should hear about an event, and hands the message to a
// Sender. Without an APNs key the sender is disabled and nothing leaves the server.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// cursorName is this consumer's row in event_cursors.
const cursorName = "notify"

// ErrDeviceGone says Apple no longer knows this token; the device stops receiving until it
// registers again.
var ErrDeviceGone = errors.New("device token is gone")

// Message is one notification.
type Message struct {
	Title string
	Body  string
	Link  string // gator://tasks/<id>
}

// Sender delivers a message to one device.
type Sender interface {
	Send(ctx context.Context, device db.Device, m Message) error
	// Name says which sender this is, for the log.
	Name() string
}

// Disabled drops messages; it is what a server without an APNs key uses.
type Disabled struct{}

// Send implements Sender.
func (Disabled) Send(context.Context, db.Device, Message) error { return nil }

// Name implements Sender.
func (Disabled) Name() string { return "disabled" }

// Notifier watches the outbox and pushes what a person would want to know.
type Notifier struct {
	pool    *pgxpool.Pool
	process *process.Service
	sender  Sender
	hub     *events.Hub
	log     *slog.Logger
	poll    time.Duration
}

// New builds a notifier; PollEvery defaults to two seconds.
func New(pool *pgxpool.Pool, proc *process.Service, sender Sender, hub *events.Hub, log *slog.Logger, poll time.Duration) *Notifier {
	if sender == nil {
		sender = Disabled{}
	}
	if poll == 0 {
		poll = 2 * time.Second
	}
	return &Notifier{pool: pool, process: proc, sender: sender, hub: hub, log: log, poll: poll}
}

// watched are the events worth a notification.
var watched = []string{"task.phase_changed", "gate.blocked", "task.requirements_changed", "job.finished"}

// Run delivers notifications until ctx ends.
func (n *Notifier) Run(ctx context.Context) {
	wake, unsub := n.hub.Subscribe("*")
	defer unsub()
	if err := db.New(n.pool).InitEventCursor(ctx, cursorName); err != nil && ctx.Err() == nil {
		n.log.Warn("notify cursor", "err", err)
	}
	n.log.Info("notifications", "sender", n.sender.Name())
	t := time.NewTicker(n.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			n.drain(ctx)
		case <-t.C:
			n.drain(ctx)
		}
	}
}

// drain delivers the events after the cursor, at least once.
func (n *Notifier) drain(ctx context.Context) {
	q := db.New(n.pool)
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
			if err := n.handle(ctx, r); err != nil && ctx.Err() == nil {
				n.log.Warn("notify", "event", r.Type, "err", err)
			}
		}
		if err := q.SetEventCursor(ctx, db.SetEventCursorParams{Name: cursorName, EventID: rows[len(rows)-1].ID}); err != nil {
			return
		}
	}
}

func (n *Notifier) handle(ctx context.Context, row db.EventsAfterRow) error {
	q := db.New(n.pool)
	taskID := row.AggregateID
	if row.Aggregate == "job" {
		job, err := q.GetJob(ctx, row.AggregateID)
		if err != nil {
			return err
		}
		if job.Status == "done" {
			return nil // finished work is not news; the inbox shows what is left
		}
		taskID = job.TaskID
	}
	if !taskID.Valid {
		return nil
	}
	task, err := q.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	message, ok, err := n.message(ctx, row, task)
	if err != nil || !ok {
		return err
	}
	users, err := n.recipients(ctx, task)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
	}
	devices, err := q.ListDevicesForUsers(ctx, users)
	if err != nil {
		return err
	}
	for _, device := range devices {
		err := n.sender.Send(ctx, device, message)
		switch {
		case errors.Is(err, ErrDeviceGone):
			if err := q.MarkDeviceGone(ctx, device.Token); err != nil {
				n.log.Warn("mark device gone", "err", err)
			}
		case err != nil:
			n.log.Warn("push", "device", device.ID, "err", err)
		}
	}
	return nil
}

// message says what happened, or reports that this event needs no person.
func (n *Notifier) message(ctx context.Context, row db.EventsAfterRow, task db.Task) (Message, bool, error) {
	var payload struct {
		To       string `json:"to"`
		Rollback bool   `json:"rollback"`
		Reason   string `json:"reason"`
	}
	_ = json.Unmarshal(row.Payload, &payload)
	m := Message{Title: task.Title, Link: "gator://tasks/" + uuidString(task.ID)}
	switch row.Type {
	case "gate.blocked":
		m.Body = "Blocked: " + payload.Reason
	case "task.requirements_changed":
		m.Body = "Requirements changed; the phase needs approving again"
	case "job.finished":
		m.Body = "A job stopped without finishing"
	case "task.phase_changed":
		detail, err := n.process.Detail(ctx, task.ID)
		if err != nil {
			return m, false, err
		}
		waits := detail.Phase.Owner == process.OwnerHuman ||
			((detail.Phase.Gate == process.GateHuman || detail.Phase.Gate == process.GateBoth) && !detail.Gate.HumanApprovedAt.Valid)
		if !waits {
			return m, false, nil // an agent takes it from here
		}
		if payload.Rollback {
			m.Body = fmt.Sprintf("Sent back to %s: %s", payload.To, payload.Reason)
		} else {
			m.Body = "Waiting for you in " + payload.To
		}
	default:
		return m, false, nil
	}
	return m, true, nil
}

// recipients are the person the task was handed to, or else everyone who can act on it.
func (n *Notifier) recipients(ctx context.Context, task db.Task) ([]pgtype.UUID, error) {
	q := db.New(n.pool)
	if task.OwnerKind != nil && *task.OwnerKind == string(process.ActorUser) && task.OwnerID.Valid {
		return []pgtype.UUID{task.OwnerID}, nil
	}
	members, err := q.ListProjectMemberIDs(ctx, task.ProjectID)
	if err != nil {
		return nil, err
	}
	admins, err := q.ListWorkspaceAdminIDs(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[[16]byte]bool{}
	var out []pgtype.UUID
	for _, id := range append(members, admins...) {
		if id.Valid && !seen[id.Bytes] {
			seen[id.Bytes] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
