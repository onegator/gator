package api

import (
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

func automationByKey(list []gen.Automation, key string) gen.Automation {
	for _, a := range list {
		if a.Key == key {
			return a
		}
	}
	return gen.Automation{}
}

// Everyone in a project sees what runs on its behalf; only its admins, and only people, switch
// anything on, and switching on one that puts agents to work needs the consent said out loud.
func TestOnlyAProjectAdminSwitchesAnAutomationOn(t *testing.T) {
	h := newHarness(t)
	adminID, adminToken, project := h.ownProject("autos")
	pid := project.Id.String()
	memberID, memberToken := h.login("member")
	h.member(pid, memberID, "member")
	viewerID, viewerToken := h.login("member")
	h.member(pid, viewerID, "viewer")
	_, strangerToken := h.login("member")

	var list []gen.Automation
	h.token = viewerToken
	if code := h.do("GET", "/projects/"+pid+"/automations", nil, &list); code != 200 || len(list) != 5 {
		t.Fatalf("a viewer reads the catalogue: %d %d", code, len(list))
	}
	for _, a := range list {
		if a.Enabled {
			t.Fatalf("every automation starts off: %+v", a)
		}
	}
	if a := automationByKey(list, "pr_review"); a.Available || a.Needs != "github" || a.UnavailableReason == nil || !a.UsesAgents {
		t.Fatalf("pr review needs GitHub: %+v", a)
	}
	if a := automationByKey(list, "weekly_summary"); !a.Available || a.UsesAgents || a.Settings.Weekday == nil || *a.Settings.Weekday != "friday" {
		t.Fatalf("the summary needs nothing and runs on Fridays: %+v", a)
	}
	h.token = strangerToken
	if code := h.do("GET", "/projects/"+pid+"/automations", nil, nil); code != 403 {
		t.Fatalf("someone outside the project: %d", code)
	}

	on := gen.AutomationChange{Enabled: true}
	for name, token := range map[string]string{"member": memberToken, "viewer": viewerToken, "stranger": strangerToken} {
		h.token = token
		if code := h.do("PUT", "/projects/"+pid+"/automations/weekly_summary", on, nil); code != 403 {
			t.Fatalf("a %s switching an automation on: %d", name, code)
		}
	}

	h.token = adminToken
	var e gen.Error
	if code := h.do("PUT", "/projects/"+pid+"/automations/nonsense", on, &e); code != 404 {
		t.Fatalf("unknown automation: %d", code)
	}
	if code := h.do("PUT", "/projects/"+pid+"/automations/pr_review", on, &e); code != 409 || e.Code == nil || *e.Code != "consent_required" {
		t.Fatalf("switching on an agent's automation without consent: %d %+v", code, e)
	}
	yes := true
	if code := h.do("PUT", "/projects/"+pid+"/automations/pr_review", gen.AutomationChange{Enabled: true, AllowAgents: &yes}, &e); code != 409 ||
		e.Code == nil || *e.Code != "automation_unavailable" {
		t.Fatalf("switching on what the project cannot run: %d %+v", code, e)
	}
	bad := "someday"
	if code := h.do("PUT", "/projects/"+pid+"/automations/weekly_summary", gen.AutomationChange{Enabled: true,
		Settings: &gen.AutomationSettings{Weekday: &bad}}, nil); code != 400 {
		t.Fatalf("a weekday that is not one: %d", code)
	}
	tuesday := "Tuesday"
	var a gen.Automation
	if code := h.do("PUT", "/projects/"+pid+"/automations/weekly_summary", gen.AutomationChange{Enabled: true,
		Settings: &gen.AutomationSettings{Weekday: &tuesday}}, &a); code != 200 {
		t.Fatalf("switching the summary on needs no consent: %d", code)
	}
	if !a.Enabled || a.EnabledBy == nil || a.EnabledBy.String() != adminID || *a.Settings.Weekday != "tuesday" || a.NextRunAt == nil {
		t.Fatalf("on, by whom, on which day, and when next: %+v", a)
	}
	if code := h.do("PUT", "/projects/"+pid+"/automations/weekly_summary", gen.AutomationChange{Enabled: false}, &a); code != 200 ||
		a.Enabled || *a.Settings.Weekday != "tuesday" {
		t.Fatalf("switching off keeps the settings: %d %+v", code, a)
	}
}
