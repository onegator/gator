package e2e

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/plugins"
)

func findDecision(ds []gen.Decision, id string) *gen.Decision {
	for i := range ds {
		if ds[i].Id == id {
			return &ds[i]
		}
	}
	return nil
}

// The full inbox holds every decision, not only the tasks: what an agent proposed, a plugin
// Gator switched off, a stalled job and a runner that lost its login. The short inbox stays
// what older clients can read.
func TestFullInboxHoldsEveryDecision(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	task := h.task()
	project := task.ProjectId.String()

	// A component and a product entry, both proposed.
	var componentID, entryID string
	if err := h.pool.QueryRow(ctx, `INSERT INTO components (project_id, key, name, status, proposed_reason)
		VALUES ($1, 'web', 'Web', 'proposed', 'jobs kept changing web/') RETURNING id::text`, project).Scan(&componentID); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(ctx, `INSERT INTO product_context (project_id, kind, title, content, version, status, created_by_kind)
		VALUES ($1, 'lesson', 'Cache keys need the locale', 'body', 1, 'proposed', 'agent') RETURNING id::text`, project).Scan(&entryID); err != nil {
		t.Fatal(err)
	}

	// A plugin Gator switched off, with the alert it raised.
	plugin := fmt.Sprintf("tripped%d", time.Now().UnixNano())
	if _, err := h.pool.Exec(ctx, `WITH p AS (INSERT INTO plugins (name, command, version, manifest) VALUES ($1, '{x}', '1', '{}') RETURNING id)
		INSERT INTO project_plugins (project_id, plugin_id, config, secrets, enabled, disabled_reason)
		SELECT $2, p.id, '{}', '\x', false, 'disabled after 5 failed calls in a row' FROM p`, plugin, project); err != nil {
		t.Fatal(err)
	}
	var alert gen.Task
	if code := h.do("POST", "/projects/"+project+"/tasks", gen.NewTask{Kind: "alert", Title: plugins.DisabledAlertTitle(plugin)}, &alert); code != 201 {
		t.Fatalf("alert: %d", code)
	}

	// A job that went quiet.
	job := h.job(task, "claude", "work", 1)
	if _, err := h.pool.Exec(ctx, `UPDATE jobs SET status = 'stalled' WHERE id = $1`, job.Id.String()); err != nil {
		t.Fatal(err)
	}

	// A runner whose claude login is gone. Its own token, so no other test's runner changes.
	hash := make([]byte, 32)
	_, _ = rand.Read(hash)
	var runnerID string
	if err := h.pool.QueryRow(ctx, `WITH tok AS (INSERT INTO api_tokens (kind, scope, hash) VALUES ('runner', 'runner', $1) RETURNING id)
		INSERT INTO runners (token_id, name, status, auth_state, capabilities, connected_at)
		SELECT tok.id, 'laptop', 'online', '{"claude":"missing","codex":"ok"}', '{"can_login":true}', now() FROM tok
		RETURNING id::text`, hash).Scan(&runnerID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.pool.Exec(ctx, `UPDATE runners SET retired_at = now(), status = 'offline' WHERE id = $1`, runnerID)
	})

	// The short inbox: tasks only, with the reasons it always had.
	short := h.decisions("?projectId=" + project)
	for _, d := range short {
		if d.Task == nil {
			t.Fatalf("the short inbox carries only tasks: %+v", d)
		}
		if d.Reason == gen.DecisionReasonJobStalled || d.Reason == gen.DecisionReasonAlert || d.Reason == gen.DecisionReasonPluginDisabled {
			t.Fatalf("an older client cannot read %s", d.Reason)
		}
		if d.Id == "" || d.Summary == "" || len(d.Actions) == 0 {
			t.Fatalf("every decision says what happened and what to do: %+v", d)
		}
	}
	if d := findDecision(short, "task:"+alert.Id.String()); d == nil || d.Reason != gen.DecisionReasonApproval {
		t.Fatalf("the short inbox shows an alert as an approval, as before: %+v", d)
	}

	full := h.decisions("?projectId=" + project + "&all=true")
	want := map[string]gen.DecisionReason{
		"component:" + componentID:         gen.DecisionReasonComponentProposed,
		"product:" + entryID:               gen.DecisionReasonProductProposed,
		"plugin:" + project + ":" + plugin: gen.DecisionReasonPluginDisabled,
		"task:" + task.Id.String():         gen.DecisionReasonJobStalled,
		"runner:" + runnerID + ":claude":   gen.DecisionReasonRunnerLogin,
	}
	for id, reason := range want {
		d := findDecision(full, id)
		if d == nil {
			t.Fatalf("missing %s in %+v", id, full)
		}
		if d.Reason != reason {
			t.Fatalf("%s: reason %s, want %s", id, d.Reason, reason)
		}
	}
	if findDecision(full, "runner:"+runnerID+":codex") != nil {
		t.Fatal("a backend that is logged in is no decision")
	}
	if findDecision(full, "task:"+alert.Id.String()) != nil {
		t.Fatal("the plugin's alert is folded into the plugin's decision, not shown twice")
	}
	p := findDecision(full, "plugin:"+project+":"+plugin)
	if p.Task == nil || p.Task.Id != alert.Id || p.Actions[0] != gen.DecisionActionEnablePlugin {
		t.Fatalf("the plugin decision carries its alert and offers to switch it on: %+v", p)
	}
	if s := findDecision(full, "task:"+task.Id.String()); s.JobId == nil || *s.JobId != job.Id || s.Urgency > 2 {
		t.Fatalf("a stalled job names its job and is urgent: %+v", s)
	}
	for i := 1; i < len(full); i++ {
		if full[i-1].Urgency > full[i].Urgency {
			t.Fatalf("most urgent first: %d before %d", full[i-1].Urgency, full[i].Urgency)
		}
	}

	// Deciding takes the decision away.
	if code := h.do("POST", "/projects/"+project+"/components/web/approve", nil, nil); code != 200 {
		t.Fatalf("approve component: %d", code)
	}
	if code := h.do("POST", "/product/"+entryID, map[string]string{"status": "approved"}, nil); code != 200 {
		t.Fatalf("approve entry: %d", code)
	}
	after := h.decisions("?projectId=" + project + "&all=true")
	if findDecision(after, "component:"+componentID) != nil || findDecision(after, "product:"+entryID) != nil {
		t.Fatal("an approved proposal no longer waits")
	}
}
