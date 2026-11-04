package automations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// A weekly automation runs once per ISO week, on its day at RunHour UTC. A server that was down
// then runs it as soon as it is back within the same week; a week that already had its run, or
// whose moment had passed before the automation was switched on, is left alone. The week's run
// is a row with a unique key, written in the same transaction as the task it made, so neither a
// restart nor two servers sharing the database can make a second one.

func weekSubject(t time.Time) string { return "week:" + Week(t) }

// nextRun is when a weekly automation that is on runs next.
func (s *Service) nextRun(ctx context.Context, q *db.Queries, row db.ProjectAutomation, day time.Weekday) (time.Time, error) {
	now := s.now()
	at := DueIn(now, day)
	if now.Before(at) {
		return at, nil
	}
	_, err := q.GetAutomationRun(ctx, db.GetAutomationRunParams{ProjectID: row.ProjectID, Automation: row.Key, Subject: weekSubject(now)})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if row.EnabledAt.Valid && row.EnabledAt.Time.After(at) {
			return at.AddDate(0, 0, 7), nil
		}
		return now, nil // due and not done: the next sweep does it
	case err != nil:
		return time.Time{}, err
	}
	return at.AddDate(0, 0, 7), nil
}

// RunScheduled runs every weekly automation that is due at now. It is safe to call as often as
// anyone likes: what already ran this week does not run again. It answers how many ran.
func (s *Service) RunScheduled(ctx context.Context, now time.Time) (int, error) {
	q := db.New(s.pool)
	rows, err := q.ListEnabledAutomations(ctx, []string{DependencyScan, WeeklySummary})
	if err != nil {
		return 0, err
	}
	ran := 0
	var errs []error
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		d, _ := Lookup(row.Key)
		var settings Settings
		_ = json.Unmarshal(row.Settings, &settings)
		settings, _ = d.Normalize(settings)
		at := DueIn(now, d.Day(settings))
		if now.Before(at) || (row.EnabledAt.Valid && row.EnabledAt.Time.After(at)) {
			continue
		}
		if !row.EnabledBy.Valid {
			s.fail(ctx, q, row.ProjectID, row.Key, "Nobody is named as having switched this on any more. Switch it off and on again.")
			continue
		}
		var made bool
		switch row.Key {
		case DependencyScan:
			made, err = s.scan(ctx, q, d, row, now)
		case WeeklySummary:
			made, err = s.summary(ctx, q, d, row, now)
		}
		if err != nil {
			s.fail(ctx, q, row.ProjectID, row.Key, "This week's run failed: "+err.Error())
			errs = append(errs, fmt.Errorf("%s in %s: %w", row.Key, uuidString(row.ProjectID), err))
			continue
		}
		if made {
			ran++
		}
	}
	return ran, errors.Join(errs...)
}

// scan opens the week's dependency scan.
func (s *Service) scan(ctx context.Context, q *db.Queries, d Definition, row db.ProjectAutomation, now time.Time) (bool, error) {
	repos, err := q.CountProjectRepos(ctx, row.ProjectID)
	if err != nil {
		return false, err
	}
	if repos == 0 {
		s.fail(ctx, q, row.ProjectID, row.Key, "The project has no repository to scan. Add one, and the next week's scan runs.")
		return false, nil
	}
	_, err = s.proc.Create(ctx, process.CreateParams{
		ProjectID: row.ProjectID, Kind: d.Kind, Title: "Dependency scan, week of " + weekStart(now).Format("2 Jan"),
		Description: scanDescription, Urgency: 3,
		Automation: automated(d, row, claim(ctx, row.ProjectID, row.Key, weekSubject(now))),
	}, process.Actor{Kind: process.ActorSystem})
	if errors.Is(err, errAlreadyRan) {
		return false, nil
	}
	return err == nil, err
}

const scanDescription = `Check this repository's dependencies for known vulnerabilities.

Use the audit tool of each ecosystem the repository uses, for example govulncheck for Go, npm audit (or pnpm audit, yarn npm audit) for JavaScript, pip-audit for Python, bundler-audit for Ruby, cargo audit for Rust. Install a tool if it is missing and you can.

Where a vulnerable dependency has a fixed version, upgrade to the smallest version that fixes it, run the tests, and commit each upgrade on its own: your work becomes a pull request. Do not upgrade anything that is not vulnerable.

In your report, list what you found: each vulnerability, the dependency and version, whether the code reaches it, and what you did about it. If you found nothing, say which tools you ran and commit nothing.`

func weekStart(t time.Time) time.Time { return DueIn(t, time.Monday).Add(-RunHour * time.Hour) }

// summary writes the week's summary. Nothing in it needs an agent: the server knows what was
// finished, what waits and what agents spent, and computing it here costs nothing and cannot
// say anything the numbers do not.
func (s *Service) summary(ctx context.Context, q *db.Queries, d Definition, row db.ProjectAutomation, now time.Time) (bool, error) {
	project, err := q.GetProject(ctx, row.ProjectID)
	if err != nil {
		return false, err
	}
	since := now.Add(-7 * 24 * time.Hour)
	doc, headline, err := s.Summarize(ctx, row.ProjectID, since, now)
	if err != nil {
		return false, err
	}
	previous, err := q.ListOpenTasksOfAutomation(ctx, db.ListOpenTasksOfAutomationParams{ProjectID: row.ProjectID, Automation: row.Key})
	if err != nil {
		return false, err
	}
	record := claim(ctx, row.ProjectID, row.Key, weekSubject(now))
	task, err := s.proc.Create(ctx, process.CreateParams{
		ProjectID: row.ProjectID, Kind: d.Kind, Title: "Week of " + since.UTC().Format("2 Jan") + " in " + project.Name,
		Description: headline + ". The summary is in this task's documents.", Urgency: 4,
		Automation: automated(d, row, func(q *db.Queries, t db.Task) error {
			if err := record(q, t); err != nil {
				return err
			}
			if _, err := q.CreateArtifact(ctx, db.CreateArtifactParams{TaskID: t.ID, Phase: t.Phase, Type: "summary", Content: &doc}); err != nil {
				return err
			}
			// The notifier tells the project's people; the event says what to tell them.
			payload, _ := json.Marshal(map[string]any{"automation": row.Key, "headline": headline, "project": project.Name})
			_, err := q.InsertEvent(ctx, db.InsertEventParams{Type: "summary.ready", Aggregate: "task", AggregateID: t.ID, Payload: payload})
			return err
		}),
	}, process.Actor{Kind: process.ActorSystem})
	if errors.Is(err, errAlreadyRan) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Last week's summary, if nobody read it, is replaced rather than left to pile up.
	for _, p := range previous {
		if p.ID == task.ID {
			continue
		}
		if _, err := s.proc.Close(ctx, p.ID, process.Actor{Kind: process.ActorSystem}, "a newer weekly summary replaced it"); err != nil {
			s.log.Warn("closing last week's summary", "task", uuidString(p.ID), "err", err)
		}
	}
	return true, nil
}

// Summarize is a project's week in Markdown, and a one-line headline for the notification.
func (s *Service) Summarize(ctx context.Context, projectID pgtype.UUID, since, until time.Time) (string, string, error) {
	q := db.New(s.pool)
	window := func() (pgtype.Timestamptz, pgtype.Timestamptz) {
		return pgtype.Timestamptz{Time: since, Valid: true}, pgtype.Timestamptz{Time: until, Valid: true}
	}
	from, to := window()
	closed, err := q.ListTasksClosedBetween(ctx, db.ListTasksClosedBetweenParams{ProjectID: projectID, Since: from, Until: to})
	if err != nil {
		return "", "", err
	}
	opened, err := q.CountTasksCreatedBetween(ctx, db.CountTasksCreatedBetweenParams{ProjectID: projectID, Since: from, Until: to})
	if err != nil {
		return "", "", err
	}
	usage, err := q.ProjectUsageBetween(ctx, db.ProjectUsageBetweenParams{ProjectID: projectID, Since: from, Until: to})
	if err != nil {
		return "", "", err
	}
	waiting, err := s.proc.Inbox(ctx, process.InboxFilter{ProjectID: projectID, All: true})
	if err != nil {
		return "", "", err
	}
	var finished []db.Task
	for _, t := range closed {
		if t.Kind != "summary" {
			finished = append(finished, t)
		}
	}
	var waits []process.Decision
	for _, w := range waiting {
		if w.Task.Kind != "summary" {
			waits = append(waits, w)
		}
	}

	const most = 15
	var b strings.Builder
	fmt.Fprintf(&b, "**%s to %s**\n\n", since.UTC().Format("Mon 2 Jan"), until.UTC().Format("Mon 2 Jan"))
	fmt.Fprintf(&b, "**Finished: %d**\n", len(finished))
	for i, t := range finished {
		if i == most {
			fmt.Fprintf(&b, "- and %d more\n", len(finished)-most)
			break
		}
		fmt.Fprintf(&b, "- %s (%s)\n", t.Title, words(t.Kind))
	}
	if len(finished) == 0 {
		b.WriteString("Nothing was finished this week.\n")
	}
	fmt.Fprintf(&b, "\n**Waiting for a person: %d**\n", len(waits))
	for i, w := range waits {
		if i == most {
			fmt.Fprintf(&b, "- and %d more\n", len(waits)-most)
			break
		}
		fmt.Fprintf(&b, "- %s: %s, since %s\n", w.Task.Title, words(string(w.Reason)), w.WaitingSince.UTC().Format("Mon 2 Jan"))
	}
	if len(waits) == 0 {
		b.WriteString("Nothing is waiting for anyone.\n")
	}
	agentTime := Duration(time.Duration(usage.AgentMs) * time.Millisecond)
	fmt.Fprintf(&b, "\n**Agents**\n%s of agent time over %d %s, costing %s.\n", agentTime, usage.Jobs, plural(int(usage.Jobs), "job", "jobs"), Money(usage.CostUsd))
	fmt.Fprintf(&b, "\n**New work**\n%d %s opened this week.\n", opened, plural(int(opened), "task was", "tasks were"))

	headline := fmt.Sprintf("%d finished, %d waiting for a person, %s on agents", len(finished), len(waits), Money(usage.CostUsd))
	return b.String(), headline, nil
}

func words(s string) string { return strings.ReplaceAll(s, "_", " ") }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Duration reads like a person says it: "3 h 12 min", "4 min", "no time".
func Duration(d time.Duration) string {
	switch {
	case d < 30*time.Second:
		if d <= 0 {
			return "no time"
		}
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(math.Round(d.Minutes())))
	}
	h := int(d.Hours())
	m := int(math.Round((d - time.Duration(h)*time.Hour).Minutes()))
	if m == 60 {
		h, m = h+1, 0
	}
	if m == 0 {
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}

// Money is dollars to the cent.
func Money(usd float64) string { return fmt.Sprintf("$%.2f", usd) }
