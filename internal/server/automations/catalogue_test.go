package automations

import (
	"testing"
	"time"
)

func TestTheWeekAndItsMoment(t *testing.T) {
	wed := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC) // a Wednesday in week 45
	if got := Week(wed); got != "2026-W45" {
		t.Fatalf("week: %s", got)
	}
	if got := DueIn(wed, time.Monday); !got.Equal(time.Date(2026, 11, 2, RunHour, 0, 0, 0, time.UTC)) {
		t.Fatalf("Monday of that week: %s", got)
	}
	if got := DueIn(wed, time.Sunday); !got.Equal(time.Date(2026, 11, 8, RunHour, 0, 0, 0, time.UTC)) {
		t.Fatalf("Sunday ends the ISO week: %s", got)
	}
	sunday := time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC)
	if DueIn(sunday, time.Friday) != DueIn(wed, time.Friday) || Week(sunday) != Week(wed) {
		t.Fatal("Sunday is still the same week")
	}
	if Week(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)) != "2026-W01" {
		t.Fatal("ISO year")
	}
}

func TestSettingsAreNormalized(t *testing.T) {
	scan, _ := Lookup(DependencyScan)
	if s, err := scan.Normalize(Settings{}); err != nil || s.Weekday != "monday" {
		t.Fatalf("default day: %+v %v", s, err)
	}
	if s, err := scan.Normalize(Settings{Weekday: " Thursday "}); err != nil || s.Weekday != "thursday" || scan.Day(s) != time.Thursday {
		t.Fatalf("named day: %+v %v", s, err)
	}
	if _, err := scan.Normalize(Settings{Weekday: "someday"}); err == nil {
		t.Fatal("not a day")
	}
	review, _ := Lookup(PRReview)
	if _, err := review.Normalize(Settings{Weekday: "monday"}); err == nil {
		t.Fatal("a pull request review has no day")
	}
	if _, ok := Lookup("nonsense"); ok {
		t.Fatal("unknown key")
	}
	for _, d := range Catalogue {
		if d.Title == "" || d.Summary == "" || d.Creates == "" || d.Kind == "" {
			t.Fatalf("incomplete entry %+v", d)
		}
		if d.Key == WeeklySummary && d.UsesAgents {
			t.Fatal("the summary is written by the server, not an agent")
		}
	}
}

func TestDurationReadsLikeAPerson(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "no time", 10 * time.Second: "under a minute", 4 * time.Minute: "4 min",
		3*time.Hour + 12*time.Minute: "3 h 12 min", 2*time.Hour + 59*time.Minute + 50*time.Second: "3 h"} {
		if got := Duration(d); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
}
