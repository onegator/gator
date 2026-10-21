package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/runners"
)

// follow subscribes to topics on the WebSocket as the current caller.
func (h *harness) follow(topics ...string) *websocket.Conn {
	h.t.Helper()
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, strings.Replace(h.srv.URL, "http", "ws", 1)+"/api/v1/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + h.token}}})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { ws.CloseNow() })
	b, _ := json.Marshal(map[string][]string{"subscribe": topics})
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		h.t.Fatal(err)
	}
	return ws
}

// await reads events until one of the given type arrives.
func await(t *testing.T, ws *websocket.Conn, want string) events.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, b, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", want, err)
		}
		var e events.Event
		_ = json.Unmarshal(b, &e)
		if e.Type == want {
			return e
		}
	}
}

func TestEditCloseAndReopenATask(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.token
	var project gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("k%d", time.Now().UnixNano()), Name: "K"}, &project); code != 201 {
		t.Fatalf("create project: %d", code)
	}
	pid := project.Id.String()
	ws := h.follow("project:" + pid)

	var task gen.Task
	if code := h.do("POST", "/projects/"+pid+"/tasks", gen.NewTask{Kind: "bug", Title: "Crash on login"}, &task); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	if e := await(t, ws, "task.created"); e.ProjectID != pid || e.AggregateID != task.Id.String() {
		t.Fatalf("the project's topic should carry its task: %+v", e)
	}
	path := "/tasks/" + task.Id.String()

	// Words and urgency change, and the trail says so.
	if code := h.do("PATCH", path, gen.TaskPatch{Title: ptr("Crash on sign-in"), Description: ptr("after the update"), Urgency: ptr(1)}, &task); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	if task.Title != "Crash on sign-in" || task.Urgency != 1 || *task.Description != "after the update" {
		t.Fatalf("patched task: %+v", task)
	}
	await(t, ws, "task.updated")
	var audited int
	if err := h.pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action = 'PATCH /api/v1/tasks/{taskId}' AND target = $1",
		"/api/v1"+path).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("audit rows for the patch: %d %v", audited, err)
	}
	var trs []gen.Transition
	h.do("GET", path+"/transitions", nil, &trs)
	if last := trs[len(trs)-1]; last.Kind != "edit" || last.ActorKind != "user" {
		t.Fatalf("the edit in the trail: %+v", last)
	}

	// Bad edits are refused without changing anything.
	if code := h.do("PATCH", path, gen.TaskPatch{Title: ptr("  ")}, nil); code != 400 {
		t.Fatalf("empty title: %d", code)
	}
	if code := h.do("PATCH", path, gen.TaskPatch{Urgency: ptr(9)}, nil); code != 400 {
		t.Fatalf("urgency off the scale: %d", code)
	}
	if code := h.do("PATCH", path, map[string]any{"phase": "approved"}, nil); code != 400 {
		t.Fatalf("the phase is not edited, it is moved: %d", code)
	}

	// Kind changes while nothing has happened, and the task starts the new process.
	kind := gen.TaskPatchKindFeature
	if code := h.do("PATCH", path, gen.TaskPatch{Kind: &kind}, &task); code != 200 || task.Kind != "feature" || task.Phase != "idea" {
		t.Fatalf("kind before work: %d %s %s", code, task.Kind, task.Phase)
	}
	var detail gen.TaskDetail
	if code := h.do("GET", path, nil, &detail); code != 200 || detail.Gate.Phase != "idea" {
		t.Fatalf("the new phase has its gate: %d %+v", code, detail.Gate)
	}
	h.do("POST", path+"/approve", nil, nil)
	if code := h.do("POST", path+"/advance", gen.Reason{}, &task); code != 200 {
		t.Fatalf("advance: %d", code)
	}
	kind = gen.TaskPatchKindChore
	if code := h.do("PATCH", path, gen.TaskPatch{Kind: &kind}, nil); code != 409 {
		t.Fatalf("kind after work started: %d", code)
	}

	// A viewer reads and does nothing else.
	viewerID, viewer := h.login("member")
	h.member(pid, viewerID, "viewer")
	h.token = viewer
	if code := h.do("PATCH", path, gen.TaskPatch{Title: ptr("mine now")}, nil); code != 403 {
		t.Fatalf("viewer patching: %d", code)
	}
	if code := h.do("POST", path+"/close", gen.TaskStateChange{Reason: "no"}, nil); code != 403 {
		t.Fatalf("viewer closing: %d", code)
	}
	h.token = admin

	// Closing takes a reason, leaves the board and keeps its history.
	if code := h.do("POST", path+"/close", gen.TaskStateChange{Reason: " "}, nil); code != 400 {
		t.Fatalf("closing without a reason: %d", code)
	}
	if code := h.do("POST", path+"/close", gen.TaskStateChange{Reason: "duplicate of another"}, &task); code != 200 || task.ClosedAt == nil {
		t.Fatalf("close: %d %+v", code, task.ClosedAt)
	}
	await(t, ws, "task.closed")
	var open []gen.Task
	h.do("GET", "/projects/"+pid+"/tasks", nil, &open)
	if len(open) != 0 {
		t.Fatalf("a closed task stays on the board: %+v", open)
	}
	if code := h.do("POST", path+"/close", gen.TaskStateChange{Reason: "again"}, nil); code != 200 {
		t.Fatalf("closing twice: %d", code)
	}
	if code := h.do("PATCH", path, gen.TaskPatch{Title: ptr("later")}, nil); code != 409 {
		t.Fatalf("editing closed work: %d", code)
	}
	if code := h.do("POST", path+"/advance", gen.Reason{}, nil); code != 409 {
		t.Fatalf("advancing closed work: %d", code)
	}

	h.token = viewer
	if code := h.do("POST", path+"/reopen", gen.TaskStateChange{Reason: "no"}, nil); code != 403 {
		t.Fatalf("viewer reopening: %d", code)
	}
	h.token = admin
	if code := h.do("POST", path+"/reopen", gen.TaskStateChange{Reason: ""}, nil); code != 400 {
		t.Fatalf("reopening without a reason: %d", code)
	}
	task = gen.Task{}
	if code := h.do("POST", path+"/reopen", gen.TaskStateChange{Reason: "not a duplicate after all"}, &task); code != 200 || task.ClosedAt != nil || task.Phase != "discovery" {
		t.Fatalf("reopen: %d %+v %s", code, task.ClosedAt, task.Phase)
	}
	await(t, ws, "task.reopened")
	h.do("GET", "/projects/"+pid+"/tasks", nil, &open)
	if len(open) != 1 || open[0].Id != task.Id {
		t.Fatalf("a reopened task is back on the board: %+v", open)
	}
	h.do("GET", path+"/transitions", nil, &trs)
	kinds := []string{}
	for _, tr := range trs {
		kinds = append(kinds, tr.Kind)
	}
	if got := strings.Join(kinds, ","); got != "create,edit,edit,advance,close,reopen" {
		t.Fatalf("the whole story stays: %s", got)
	}
}

func TestReopenedPhaseNeedsApprovalAgain(t *testing.T) {
	h := newHarness(t)
	var project gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("ra%d", time.Now().UnixNano()), Name: "RA"}, &project)
	var task gen.Task
	h.do("POST", "/projects/"+project.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "x"}, &task)
	path := "/tasks/" + task.Id.String()
	h.do("POST", path+"/approve", nil, nil)
	h.do("POST", path+"/close", gen.TaskStateChange{Reason: "done elsewhere"}, nil)
	h.do("POST", path+"/reopen", gen.TaskStateChange{Reason: "it was not"}, nil)
	var detail gen.TaskDetail
	h.do("GET", path, nil, &detail)
	if detail.Gate.HumanApproved {
		t.Fatal("whoever reopens a task says it is not done; the old approval must not carry it on")
	}
}

func TestProjectTopicIsForItsReadersOnly(t *testing.T) {
	h := newHarness(t)
	var project gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("pt%d", time.Now().UnixNano()), Name: "PT"}, &project)
	_, stranger := h.login("member")
	h.token = stranger
	ws := h.follow("project:" + project.Id.String())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := ws.Read(ctx)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.StatusPolicyViolation {
		t.Fatalf("a stranger following a project: %v", err)
	}
}

func TestClosingATaskStopsItsJobs(t *testing.T) {
	h := newHarness(t)
	h.api.Runners = runners.New(h.pool, h.api.Process, h.api.Hub, slog.Default(), runners.Config{})
	var project gen.Project
	h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("cj%d", time.Now().UnixNano()), Name: "CJ"}, &project)
	var task gen.Task
	h.do("POST", "/projects/"+project.Id.String()+"/tasks", gen.NewTask{Kind: "bug", Title: "x"}, &task)
	path := "/tasks/" + task.Id.String()
	var job gen.Job
	if code := h.do("POST", path+"/jobs", gen.NewJob{Backend: ptr("claude")}, &job); code != 201 || job.Status != "queued" {
		t.Fatalf("queue a job: %d %s", code, job.Status)
	}
	h.do("POST", path+"/close", gen.TaskStateChange{Reason: "not wanted"}, nil)
	h.do("GET", "/jobs/"+job.Id.String(), nil, &job)
	if job.Status != "stopped" || job.StopReason == nil || !strings.Contains(*job.StopReason, "not wanted") {
		t.Fatalf("a closed task's job keeps waiting for a runner: %s %v", job.Status, job.StopReason)
	}
}
