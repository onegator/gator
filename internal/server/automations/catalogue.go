// Package automations is the fixed catalogue of things Gator does by itself for a project once a
// person switches them on: review a pull request, scan dependencies every week, look into an
// error from production, write the week's summary. Each one is off until a project admin turns
// it on, and turning on one that puts agents to work is that person allowing agents to work
// without asking on every task it creates.
package automations

import (
	"fmt"
	"strings"
	"time"
)

// Keys of the catalogue. They are stored and sent as they are, so they never change.
const (
	PRReview       = "pr_review"
	SecurityReview = "security_review"
	DependencyScan = "dependency_scan"
	ErrorTriage    = "error_triage"
	WeeklySummary  = "weekly_summary"
)

// What sets an automation off.
const (
	TriggerPullRequest = "pull_request"
	TriggerWeekly      = "weekly"
	TriggerIncident    = "incident"
)

// What a project needs before an automation can do anything.
const (
	NeedsNothing    = ""
	NeedsGitHub     = "github"     // a plugin with the vcs capability: GitHub
	NeedsMonitoring = "monitoring" // a plugin with the monitoring capability: Honeybadger, Sentry
	NeedsRepository = "repository" // a repository registered on the project
)

// Definition is one entry of the catalogue.
type Definition struct {
	Key   string
	Title string
	// Summary is the line on the card.
	Summary string
	// Creates says what switching it on will make, for the person deciding.
	Creates string
	Trigger string
	Needs   string
	// Kind is the kind of task it creates.
	Kind string
	// UsesAgents says whether its tasks are worked on by agents. Only those need the consent.
	UsesAgents bool
	// Weekday is the default day of a weekly automation.
	Weekday time.Weekday
}

// Catalogue is every automation, in the order a person reads them.
var Catalogue = []Definition{
	{
		Key: PRReview, Title: "PR review", Trigger: TriggerPullRequest, Needs: NeedsGitHub, Kind: "review", UsesAgents: true,
		Summary: "An agent reads every pull request a person opens and points out bugs.",
		Creates: "A review task for each pull request a person opens or pushes to. A reviewer agent reads the change " +
			"and what it writes is posted on the pull request as a review comment.",
	},
	{
		Key: SecurityReview, Title: "Security review", Trigger: TriggerPullRequest, Needs: NeedsGitHub, Kind: "security_review", UsesAgents: true,
		Summary: "An agent reads every pull request for what an attacker could use.",
		Creates: "A security review task for each pull request a person opens or pushes to. An agent looks for injection, " +
			"leaked secrets and broken access checks, and posts what it finds on the pull request.",
	},
	{
		Key: DependencyScan, Title: "Dependency scan", Trigger: TriggerWeekly, Needs: NeedsRepository, Kind: "chore", UsesAgents: true,
		Weekday: time.Monday,
		Summary: "Once a week, an agent checks the dependencies for known vulnerabilities.",
		Creates: "A chore task once a week. An agent runs the repository's own audit tool (govulncheck, npm audit, pip-audit, " +
			"bundler-audit…) and, when it can fix what it finds, its work becomes a pull request like any other.",
	},
	{
		Key: ErrorTriage, Title: "Error triage", Trigger: TriggerIncident, Needs: NeedsMonitoring, Kind: "incident", UsesAgents: true,
		Summary: "An agent looks into each new error from production and proposes a fix.",
		Creates: "Nothing new: the incident task a monitoring tool opens gets an agent straight away, which finds the cause " +
			"and proposes a fix. With this off, incident tasks wait for a person as they do now.",
	},
	{
		Key: WeeklySummary, Title: "Weekly summary", Trigger: TriggerWeekly, Needs: NeedsNothing, Kind: "summary",
		Weekday: time.Friday,
		Summary: "A short summary of the project's week, with a notification.",
		Creates: "A summary once a week: what was finished, what waits for a person, and what agents spent in time and money. " +
			"Gator writes it from what it already knows; no agent is involved.",
	},
}

// Lookup finds a definition by key.
func Lookup(key string) (Definition, bool) {
	for _, d := range Catalogue {
		if d.Key == key {
			return d, true
		}
	}
	return Definition{}, false
}

// Weekly says whether it runs on a day of the week.
func (d Definition) Weekly() bool { return d.Trigger == TriggerWeekly }

// Settings are what a person can choose about an automation. Only weekly ones have any.
type Settings struct {
	// Weekday is the lowercase English name of the day a weekly automation runs on.
	Weekday string `json:"weekday,omitempty"`
}

var weekdays = map[string]time.Weekday{"monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday, "sunday": time.Sunday}

// WeekdayName is the stored form of a day.
func WeekdayName(d time.Weekday) string { return strings.ToLower(d.String()) }

// Normalize fills in the defaults and refuses what does not fit the automation.
func (d Definition) Normalize(s Settings) (Settings, error) {
	if !d.Weekly() {
		if s.Weekday != "" {
			return s, fmt.Errorf("%s does not run on a day of the week", d.Title)
		}
		return Settings{}, nil
	}
	day := strings.ToLower(strings.TrimSpace(s.Weekday))
	if day == "" {
		return Settings{Weekday: WeekdayName(d.Weekday)}, nil
	}
	if _, ok := weekdays[day]; !ok {
		return s, fmt.Errorf("weekday %q is not a day of the week", s.Weekday)
	}
	return Settings{Weekday: day}, nil
}

// Day is the day a weekly automation runs on.
func (d Definition) Day(s Settings) time.Weekday {
	if day, ok := weekdays[s.Weekday]; ok {
		return day
	}
	return d.Weekday
}

// RunHour is the hour, UTC, a weekly automation runs at on its day: early enough in Europe to
// be there with the morning's coffee, and a working day in the Americas.
const RunHour = 9

// Week names the ISO week t falls in, which is what a weekly run is counted by: "2026-W45".
func Week(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// DueIn is the moment in t's week (Monday to Sunday, UTC) that a run on day is due.
func DueIn(t time.Time, day time.Weekday) time.Time {
	t = t.UTC()
	monday := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -int((t.Weekday()+6)%7))
	return monday.AddDate(0, 0, int((day+6)%7)).Add(RunHour * time.Hour)
}
