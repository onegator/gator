package plugins

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// defaultObservation is how long a release is watched when the plugin names no window. A
// deploy is not finished the moment it lands; most faults show up in the first hour of real
// traffic, and a task that closes before then closes on a guess.
const defaultObservation = time.Hour

// monitoringPhase is the phase whose exit condition is "the window passed and nothing was
// reported". Settling only ever moves a task out of this one.
const monitoringPhase = "monitoring"

// releasePhase is the phase a deploy plugin owns; recording a release is its exit.
const releasePhase = "release"

// recordRelease stores what reached an environment and starts its observation window.
func (i *instance) recordRelease(ctx context.Context, q *db.Queries, p plugin.ReleaseRecordParams) (plugin.ReleaseRecordResult, error) {
	version := strings.TrimSpace(p.Version)
	if version == "" {
		return plugin.ReleaseRecordResult{}, plugin.Errorf(plugin.CodeInvalidParams, "a release needs a version")
	}
	env := strings.TrimSpace(p.Environment)
	if env == "" {
		env = "production"
	}
	var taskID pgtype.UUID
	if p.TaskID != "" {
		t, err := i.task(ctx, q, p.TaskID)
		if err != nil {
			return plugin.ReleaseRecordResult{}, err
		}
		taskID = t.ID
	}
	window := time.Duration(p.ObserveMinutes) * time.Minute
	if p.ObserveMinutes == 0 {
		window = defaultObservation
	}
	var until pgtype.Timestamptz
	if window > 0 {
		until = pgtype.Timestamptz{Time: time.Now().Add(window), Valid: true}
	}
	r, err := q.RecordRelease(ctx, db.RecordReleaseParams{
		ProjectID: i.b.projectID, TaskID: taskID, Version: version, CommitSha: p.CommitSHA,
		Url: p.URL, Environment: env, SourcePluginID: pgtype.UUID{Bytes: i.b.id.Bytes, Valid: true},
		ObservationUntil: until,
	})
	if err != nil {
		return plugin.ReleaseRecordResult{}, err
	}
	// Recording the release is what finishes the Release phase: the plugin has done the work
	// that phase names. Without this the task waits in Release for a person to confirm what
	// the deploy plugin already told us.
	if r.TaskID.Valid {
		if t, err := q.GetTask(ctx, r.TaskID); err == nil && t.Phase == releasePhase && !t.ClosedAt.Valid {
			if _, err := i.h.proc.Advance(ctx, t.ID, process.Actor{Kind: process.ActorPlugin}, "released "+r.Version); err != nil {
				i.log().Warn("advancing after a release", "task", uuidString(t.ID), "err", err)
			}
		}
	}
	_ = emit(ctx, q, "release.recorded", "release", r.ID, map[string]any{
		"project": uuidString(i.b.projectID), "version": r.Version, "environment": r.Environment,
		"task_id": uuidString(r.TaskID),
	})
	return plugin.ReleaseRecordResult{ReleaseID: uuidString(r.ID)}, nil
}

// upsertIncident records a fault in production. A new one opens an incident task; a repeat
// only raises the count, because a monitoring tool saying the same thing a thousand times is
// still one thing wrong.
func (i *instance) upsertIncident(ctx context.Context, q *db.Queries, p plugin.IncidentUpsertParams) (plugin.IncidentUpsertResult, error) {
	fingerprint := strings.TrimSpace(p.Fingerprint)
	title := strings.TrimSpace(p.Title)
	if fingerprint == "" || title == "" {
		return plugin.IncidentUpsertResult{}, plugin.Errorf(plugin.CodeInvalidParams, "an incident needs a fingerprint and a title")
	}
	severity := strings.ToLower(strings.TrimSpace(p.Severity))
	switch severity {
	case "critical", "high", "medium", "low":
	case "":
		severity = "medium"
	default:
		return plugin.IncidentUpsertResult{}, plugin.Errorf(plugin.CodeInvalidParams, "severity %q is not one of critical, high, medium, low", p.Severity)
	}
	var releaseID pgtype.UUID
	if p.ReleaseVersion != "" {
		if r, err := q.NewestRelease(ctx, db.NewestReleaseParams{ProjectID: i.b.projectID, Environment: "production"}); err == nil && r.Version == p.ReleaseVersion {
			releaseID = r.ID
		}
	}
	row, err := q.UpsertIncident(ctx, db.UpsertIncidentParams{
		ProjectID: i.b.projectID, SourcePluginID: pgtype.UUID{Bytes: i.b.id.Bytes, Valid: true},
		ExternalID: p.ExternalID, Fingerprint: fingerprint, Title: title, Severity: severity,
		Url: p.URL, ReleaseID: releaseID,
	})
	if err != nil {
		return plugin.IncidentUpsertResult{}, err
	}
	out := plugin.IncidentUpsertResult{IncidentID: uuidString(row.ID), Created: row.IsNew, TaskID: uuidString(row.TaskID)}
	if !row.IsNew && row.TaskID.Valid {
		return out, nil // already someone's problem
	}
	task, err := i.h.proc.Create(ctx, process.CreateParams{
		ProjectID: i.b.projectID, Kind: "incident", Title: title,
		Description:  incidentDescription(p, int(row.Count)),
		Urgency:      int16(urgencyFor(severity)),
		OriginSource: i.b.name,
	}, process.Actor{Kind: process.ActorPlugin})
	if err != nil {
		return out, plugin.Errorf(plugin.CodeInvalidParams, "%v", err)
	}
	if err := q.SetIncidentTask(ctx, db.SetIncidentTaskParams{ID: row.ID, TaskID: task.ID}); err != nil {
		return out, err
	}
	out.TaskID = uuidString(task.ID)
	return out, nil
}

// incidentDescription says where it came from, so whoever opens the task can go and look.
func incidentDescription(p plugin.IncidentUpsertParams, seen int) string {
	var b strings.Builder
	b.WriteString("Reported from production.\n\n")
	if p.URL != "" {
		b.WriteString(p.URL + "\n\n")
	}
	if p.ReleaseVersion != "" {
		b.WriteString("After release " + p.ReleaseVersion + ".\n\n")
	}
	if seen > 1 {
		b.WriteString("Seen ")
		b.WriteString(plural(seen))
		b.WriteString(" so far.\n")
	}
	b.WriteString("Fingerprint: " + p.Fingerprint)
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return "once"
	}
	return strconv.Itoa(n) + " times"
}

// urgencyFor maps a monitoring tool's severity onto the inbox's ordering: 1 is most urgent.
func urgencyFor(severity string) int {
	switch severity {
	case "critical":
		return 1
	case "high":
		return 2
	case "low":
		return 4
	default:
		return 3
	}
}

// SettleReleases finishes the tasks of releases whose observation window has run out without
// an open incident against them. A window that nobody acts on is the whole promise of the
// Monitoring phase: nothing said means it held.
func (h *Host) SettleReleases(ctx context.Context, now time.Time, max int32) (settled int, err error) {
	if max <= 0 {
		max = 50
	}
	q := db.New(h.pool)
	rows, err := q.ListSettledReleases(ctx, db.ListSettledReleasesParams{
		Now: pgtype.Timestamptz{Time: now, Valid: true}, MaxRows: max})
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if ctx.Err() != nil {
			break
		}
		open, err := q.CountOpenIncidentsForRelease(ctx, r.ID)
		if err != nil {
			return settled, err
		}
		if open > 0 {
			// Something is wrong in production. The window does not close over it; a person
			// decides what to do with the incident task first.
			h.log.Info("release still has open incidents", "version", r.Version, "incidents", open)
			continue
		}
		if err := q.SettleRelease(ctx, r.ID); err != nil {
			return settled, err
		}
		settled++
		if !r.TaskID.Valid {
			continue
		}
		t, err := q.GetTask(ctx, r.TaskID)
		if err != nil || t.ClosedAt.Valid {
			continue
		}
		// Only a task that is actually being watched. A release recorded against work that
		// has moved on elsewhere is still worth storing, but it is not a reason to advance
		// a phase nobody was waiting on.
		if t.Phase != monitoringPhase {
			continue
		}
		if _, err := h.proc.Advance(ctx, t.ID, process.Actor{Kind: process.ActorSystem},
			"observed for "+r.ObservationUntil.Time.Sub(r.DeployedAt.Time).String()+" with nothing reported"); err != nil {
			h.log.Warn("closing a watched task", "task", uuidString(t.ID), "err", err)
		}
	}
	return settled, nil
}

// closeIncident records that a fault has stopped. The task it opened is left alone: whether
// the fix is finished is a person's call, and a monitoring tool going quiet is not that. But
// the release it was blaming can be settled again, which is the part nobody could do by hand.
func (i *instance) closeIncident(ctx context.Context, q *db.Queries, p plugin.IncidentCloseParams) error {
	fingerprint := strings.TrimSpace(p.Fingerprint)
	if fingerprint == "" {
		return plugin.Errorf(plugin.CodeInvalidParams, "closing an incident needs its fingerprint")
	}
	row, err := q.CloseIncident(ctx, db.CloseIncidentParams{ProjectID: i.b.projectID, Fingerprint: fingerprint})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already closed, or never ours: saying so twice is not an error
	}
	if err != nil {
		return err
	}
	return emit(ctx, q, "incident.closed", "incident", row.ID, map[string]any{
		"project": uuidString(i.b.projectID), "fingerprint": fingerprint, "title": row.Title})
}
