package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/runners"
)

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatal(err)
	}
	return u
}

// The inbox, a task's topic and the firehose route by topic alone. Before, they carried every
// project's events to anyone signed in; now a stranger to a project hears nothing of it there
// either, while their own project's events still arrive.
func TestEveryTopicKeepsToTheProjectsTheCallerReads(t *testing.T) {
	h := newHarness(t)
	admin := h.token
	var theirs gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("vis-a%d", time.Now().UnixNano()), Name: "A"}, &theirs)
	var secret gen.Task
	if code := h.do("POST", "/projects/"+theirs.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "secret"}, &secret); code != 201 {
		t.Fatalf("create task: %d", code)
	}

	_, stranger := h.login("member")
	h.token = stranger
	var own gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("vis-b%d", time.Now().UnixNano()), Name: "B"}, &own); code != 201 {
		t.Fatalf("a member creates a project: %d", code)
	}
	for _, topics := range [][]string{{"inbox"}, {"task:" + secret.Id.String()}, {"*"}} {
		h.token = stranger
		ws := h.follow(topics...)
		time.Sleep(100 * time.Millisecond) // the subscription is in place before anything happens

		h.token = admin
		if code := h.do("PATCH", "/tasks/"+secret.Id.String(), gen.TaskPatch{Title: ptr("still secret " + topics[0])}, nil); code != 200 {
			t.Fatalf("patch: %d", code)
		}
		h.do("POST", "/projects/"+theirs.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "another secret"}, nil)
		h.token = stranger
		var mine gen.Task
		h.do("POST", "/projects/"+own.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "mine"}, &mine)

		// Events arrive in outbox order, so anything of project A would come before this.
		for {
			e, ok := nextEvent(t, ws, 2*time.Second)
			if !ok {
				if topics[0] == "task:"+secret.Id.String() {
					break // the task's own topic stays silent for a stranger
				}
				t.Fatalf("%v: the caller's own task.created never arrived", topics)
			}
			if e.ProjectID == theirs.Id.String() || e.AggregateID == secret.Id.String() {
				t.Fatalf("%v carried %s from a project the caller cannot read", topics, e.Type)
			}
			if e.Type == "task.created" && e.AggregateID == mine.Id.String() {
				break
			}
		}
	}

	// A member of the project hears it on the same topics.
	h.token = admin
	memberID, member := h.login("member")
	h.member(theirs.Id.String(), memberID, "viewer")
	h.token = member
	ws := h.follow("inbox")
	time.Sleep(100 * time.Millisecond)
	h.token = admin
	h.do("PATCH", "/tasks/"+secret.Id.String(), gen.TaskPatch{Title: ptr("for the viewer")}, nil)
	if e := await(t, ws, "task.updated"); e.AggregateID != secret.Id.String() {
		t.Fatalf("a viewer should hear about their project: %+v", e)
	}
}

// nextEvent reads the next event, or answers false when none arrives within wait.
func nextEvent(t *testing.T, ws *websocket.Conn, wait time.Duration) (events.Event, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, b, err := ws.Read(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return events.Event{}, false
		}
		t.Fatalf("reading the socket: %v", err)
	}
	var e events.Event
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e, true
}

// Job events (an agent's live output among them) and runner events are filtered by the same
// rule, for people, runners and plugins.
func TestEventFilterRules(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.api.Runners = runners.New(h.pool, h.api.Process, h.api.Hub, slog.Default(), runners.Config{})
	var a, b gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("fa%d", time.Now().UnixNano()), Name: "A"}, &a)
	h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("fb%d", time.Now().UnixNano()), Name: "B"}, &b)
	var task gen.Task
	h.do("POST", "/projects/"+a.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "x"}, &task)
	var job gen.Job
	if code := h.do("POST", "/tasks/"+task.Id.String()+"/jobs", gen.NewJob{Backend: ptr("claude")}, &job); code != 201 {
		t.Fatalf("queue a job: %d", code)
	}
	output := events.Event{Type: "job.output", Aggregate: "job", AggregateID: job.Id.String()}
	created := events.Event{Type: "task.created", Aggregate: "task", AggregateID: task.Id.String(), ProjectID: a.Id.String()}
	runnerOnline := events.Event{Type: "runner.online", Aggregate: "runner", AggregateID: "00000000-0000-0000-0000-000000000001"}
	ghost := events.Event{Type: "task.created", Aggregate: "task", AggregateID: "00000000-0000-0000-0000-0000000000ff"}

	strangerID, _ := h.login("member")
	viewerID, _ := h.login("member")
	h.member(a.Id.String(), viewerID, "viewer")
	user := func(id string) auth.Principal {
		return auth.Principal{Kind: auth.KindUser, UserID: mustUUID(t, id), WorkspaceRole: "member"}
	}
	cases := []struct {
		name string
		p    auth.Principal
		e    events.Event
		want bool
	}{
		{"stranger, job output", user(strangerID), output, false},
		{"stranger, task event", user(strangerID), created, false},
		{"stranger, runner event", user(strangerID), runnerOnline, true},
		{"viewer, job output", user(viewerID), output, true},
		{"viewer, task event", user(viewerID), created, true},
		{"viewer, task that no longer exists", user(viewerID), ghost, false},
		{"workspace admin", auth.Principal{Kind: auth.KindUser, WorkspaceRole: "admin"}, output, true},
		{"runner", auth.Principal{Kind: auth.KindRunner}, output, true},
		{"one-job cloud runner", auth.Principal{Kind: auth.KindRunner, JobID: mustUUID(t, job.Id.String())}, output, false},
		{"plugin of the project", auth.Principal{Kind: auth.KindPlugin, ProjectID: mustUUID(t, a.Id.String())}, output, true},
		{"plugin of another project", auth.Principal{Kind: auth.KindPlugin, ProjectID: mustUUID(t, b.Id.String())}, output, false},
	}
	for _, c := range cases {
		if got := newEventFilter(h.api, c.p).allow(ctx, c.e); got != c.want {
			t.Errorf("%s: allow = %v, want %v", c.name, got, c.want)
		}
	}

	// Someone removed from the project stops hearing about it once the answer goes stale.
	f := newEventFilter(h.api, user(viewerID))
	now := time.Now()
	f.now = func() time.Time { return now }
	if !f.allow(ctx, created) {
		t.Fatal("a viewer hears about their project")
	}
	if _, err := h.pool.Exec(ctx, "DELETE FROM memberships WHERE project_id=$1 AND user_id=$2", a.Id.String(), viewerID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(visibilityTTL)
	if f.allow(ctx, created) {
		t.Fatal("a removed member should stop hearing about the project")
	}
}
