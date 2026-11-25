package conversations

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// cardMessage is a card as it sits in the database.
func cardMessage(t *testing.T, c Card) db.Message {
	t.Helper()
	b, err := json.Marshal(c.Data)
	if err != nil {
		t.Fatal(err)
	}
	return db.Message{Kind: messageCard, CardType: &c.Type, CardKey: &c.Key, Body: c.Body, Card: b}
}

func TestDecisionUpdates(t *testing.T) {
	task := db.Task{ID: newID(1), Title: "Plan the login", Phase: "planning", PhaseEnteredAt: ts(time.Unix(1000, 0))}
	waiting := process.Decision{Task: task, Reason: process.ReasonApproval}
	now := DecisionCard(waiting)

	moved := task
	moved.Phase, moved.PhaseEnteredAt = "implementation", ts(time.Unix(2000, 0))
	later := DecisionCard(process.Decision{Task: moved, Reason: process.ReasonApproval})

	resolvedBefore := now
	resolvedBefore.Data = map[string]any{"state": decisionResolved}

	for _, tc := range []struct {
		name     string
		d        process.Decision
		waiting  bool
		existing []db.Message
		want     map[string]string // key -> state
	}{
		{"a new wait is one pending card", waiting, true, nil, map[string]string{now.Key: decisionPending}},
		{"the same wait again is the same card", waiting, true, []db.Message{cardMessage(t, now)}, map[string]string{now.Key: decisionPending}},
		{"approved: the card says it was decided", waiting, false, []db.Message{cardMessage(t, now)}, map[string]string{now.Key: decisionResolved}},
		{"moved on to another wait: old decided, new pending", process.Decision{Task: moved, Reason: process.ReasonApproval}, true,
			[]db.Message{cardMessage(t, now)}, map[string]string{now.Key: decisionResolved, later.Key: decisionPending}},
		{"a card already decided is left alone", waiting, false, []db.Message{cardMessage(t, resolvedBefore)}, map[string]string{}},
		{"nothing waits and nothing was said", waiting, false, nil, map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			for _, c := range decisionUpdates(tc.d, tc.waiting, tc.existing) {
				if c.Type != CardDecision {
					t.Fatalf("type %q", c.Type)
				}
				if _, twice := got[c.Key]; twice {
					t.Fatalf("card %s answered twice", c.Key)
				}
				got[c.Key], _ = c.Data["state"].(string)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("cards %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("cards %v, want %v", got, tc.want)
				}
			}
		})
	}
	if now.Key == later.Key {
		t.Fatal("two waits share a card")
	}
}

func TestDecisionCardOffersTheTasksOwnActions(t *testing.T) {
	task := db.Task{ID: newID(1), Title: "T", Phase: "planning"}
	for _, tc := range []struct {
		reason process.DecisionReason
		allow  bool
		want   []string
	}{
		{process.ReasonApproval, false, []string{"approve", "rollback", "handoff", "allow_agents"}},
		{process.ReasonApproval, true, []string{"approve", "rollback", "handoff"}},
		{process.ReasonExternal, false, []string{"admit", "close"}},
		{process.ReasonBlocked, false, []string{"rollback", "handoff"}},
	} {
		tk := task
		if tc.allow {
			tk.AgentsAllowedAt = ts(time.Now())
		}
		c := DecisionCard(process.Decision{Task: tk, Reason: tc.reason})
		got, _ := c.Data["actions"].([]string)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: actions %v, want %v", tc.reason, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: actions %v, want %v", tc.reason, got, tc.want)
			}
		}
		ref, _ := c.Data["ref"].(map[string]any)
		if ref["kind"] != "task" || ref["id"] != uuidString(task.ID) {
			t.Fatalf("ref %v", ref)
		}
	}
}

// One job keeps one card from queued to done: the key never changes with its status.
func TestJobCardFollowsOneJob(t *testing.T) {
	job := db.Job{ID: newID(5), TaskID: newID(1), Phase: "implementation", Role: "worker", Backend: "claude"}
	var keys []string
	for _, tc := range []struct {
		status string
		stop   bool
	}{{"queued", true}, {"running", true}, {"stalled", true}, {"failed", false}, {"done", false}} {
		job.Status = tc.status
		c := JobCard(job, nil)
		keys = append(keys, c.Key)
		actions, _ := c.Data["actions"].([]string)
		hasStop := len(actions) > 0 && actions[0] == "stop"
		if hasStop != tc.stop || c.Data["state"] != tc.status {
			t.Fatalf("%s: %+v", tc.status, c.Data)
		}
	}
	for _, k := range keys {
		if k != keys[0] {
			t.Fatalf("keys %v", keys)
		}
	}
}

// A job card says whose work it is: the agent's handle, not just a role.
func TestJobCardNamesItsAgent(t *testing.T) {
	job := db.Job{ID: newID(6), TaskID: newID(1), Phase: "planning", Role: "planner", Backend: "claude", Status: "running"}
	planner := db.Agent{ID: newID(7), Handle: "planner", DisplayName: "Planner"}
	c := JobCard(job, &planner)
	agent, _ := c.Data["agent"].(map[string]any)
	if c.Data["title"] != "@planner" || agent["handle"] != "planner" || !strings.Contains(c.Body, "@planner") {
		t.Fatalf("card: %s %+v", c.Body, c.Data)
	}
	if _, ok := JobCard(job, nil).Data["agent"]; ok {
		t.Fatal("a job nobody signed names no agent")
	}
}

func TestArtifactCardIsTheNewestVersion(t *testing.T) {
	all := []db.Artifact{
		{ID: newID(1), Phase: "planning", Type: "plan", Version: 1},
		{ID: newID(2), Phase: "planning", Type: "plan", Version: 3},
		{ID: newID(3), Phase: "planning", Type: "plan", Version: 2},
		{ID: newID(4), Phase: "discovery", Type: "brief", Version: 9},
	}
	a, ok := newest(all, "planning", "plan")
	if !ok || a.Version != 3 {
		t.Fatalf("newest: %+v %v", a, ok)
	}
	c := ArtifactCard(a)
	if c.Key != ArtifactCard(all[0]).Key || c.Data["state"] != "v3" {
		t.Fatalf("card %+v", c)
	}
	if actions, _ := c.Data["actions"].([]string); len(actions) != 2 {
		t.Fatalf("a third version can be diffed: %v", actions)
	}
}

func TestSameJSONReadsMeaningNotSpelling(t *testing.T) {
	if !sameJSON([]byte(`{"a": 1, "b": [1, 2]}`), []byte(`{"b":[1,2],"a":1}`)) {
		t.Fatal("the same document spelled twice differs")
	}
	if sameJSON([]byte(`{"a":1}`), []byte(`{"a":2}`)) {
		t.Fatal("different documents are the same")
	}
}

// fakeTasks and fakeJobs stand in for the store.
type fakeTasks map[[16]byte]db.Task

func (f fakeTasks) GetTask(_ context.Context, id pgtype.UUID) (db.Task, error) {
	t, ok := f[id.Bytes]
	if !ok {
		return t, pgx.ErrNoRows
	}
	return t, nil
}

type fakeJobs struct {
	fakeTasks
	jobs    map[[16]byte]db.Job
	changes map[[16]byte][][]byte
}

func (f fakeJobs) GetJob(_ context.Context, id pgtype.UUID) (db.Job, error) {
	j, ok := f.jobs[id.Bytes]
	if !ok {
		return j, pgx.ErrNoRows
	}
	return j, nil
}

func (f fakeJobs) GetAgent(context.Context, pgtype.UUID) (db.Agent, error) {
	return db.Agent{}, pgx.ErrNoRows
}

func (f fakeJobs) JobProgress(context.Context, pgtype.UUID) (db.JobProgressRow, error) {
	return db.JobProgressRow{}, nil
}

func (f fakeJobs) JobStartupPhase(context.Context, pgtype.UUID) (string, error) { return "", nil }

func (f fakeJobs) JobChanges(_ context.Context, id pgtype.UUID) ([][]byte, error) {
	if f.changes != nil {
		return f.changes[id.Bytes], nil
	}
	return nil, nil
}

func TestSourcesLeaveNoCardForWhatIsGone(t *testing.T) {
	ctx := context.Background()
	task := db.Task{ID: newID(1), Title: "T", Kind: "bug", Phase: "planning"}
	tasks := fakeTasks{task.ID.Bytes: task}
	got, err := taskCards{tasks: tasks}.Cards(ctx, db.EventsAfterRow{AggregateID: task.ID})
	if err != nil || len(got) != 1 || !got[0].Root || got[0].Card.Key != taskCardKey(task) {
		t.Fatalf("task card: %+v %v", got, err)
	}
	if got, err := (taskCards{tasks: tasks}).Cards(ctx, db.EventsAfterRow{AggregateID: newID(9)}); err != nil || len(got) != 0 {
		t.Fatalf("a deleted task: %+v %v", got, err)
	}
	jobs := fakeJobs{fakeTasks: tasks, jobs: map[[16]byte]db.Job{newID(5).Bytes: {ID: newID(5), TaskID: task.ID, Status: "queued"}}}
	got, err = jobCards{jobs: jobs}.Cards(ctx, db.EventsAfterRow{AggregateID: newID(5)})
	if err != nil || len(got) != 1 || got[0].Root || got[0].Task.ID != task.ID {
		t.Fatalf("job card: %+v %v", got, err)
	}
	if got, err := (jobCards{jobs: jobs}).Cards(ctx, db.EventsAfterRow{AggregateID: newID(6)}); err != nil || len(got) != 0 {
		t.Fatalf("a deleted job: %+v %v", got, err)
	}
}

// Whatever can start or end a wait reaches the decision card, the runner's sweep included, and
// a job nobody can take says so on its card.
func TestCardsHearTheSweep(t *testing.T) {
	for _, src := range []struct {
		name   string
		events []string
		want   []string
	}{
		{"decision", decisionCards{}.Events(), []string{"task.job_no_runner", "task.job_stalled", "task.job_requeued"}},
		{"job", jobCards{}.Events(), []string{"job.no_runner", "job.requeued", "job.stalled"}},
	} {
		for _, w := range src.want {
			if !slices.Contains(src.events, w) {
				t.Errorf("%s cards miss %s", src.name, w)
			}
		}
	}
	job := db.Job{ID: newID(5), Status: "queued", UnassignableSince: ts(time.Now())}
	if c := JobCard(job, nil); c.Data["state"] != "no_runner" {
		t.Fatalf("a job no runner can take: %v", c.Data["state"])
	}
}

// pinCards is a kind of card the package does not know.
type pinCards struct{}

func (pinCards) Events() []string { return []string{"pin.added"} }
func (pinCards) Cards(context.Context, db.EventsAfterRow) ([]PlacedCard, error) {
	return nil, nil
}

// A new kind of card is one more source in Deps; the built-in ones stay.
func TestCardSourcesAreAdditive(t *testing.T) {
	m := New(nil, Deps{CardSources: []CardSource{pinCards{}}})
	sources := m.CardSources()
	if len(sources) != 4 {
		t.Fatalf("%d sources, want the three built in (no decisions without Deps.Decisions) and the pin", len(sources))
	}
	if _, ok := sources[len(sources)-1].(pinCards); !ok {
		t.Fatalf("the added source is not last: %T", sources[len(sources)-1])
	}
	c := m.Consumer(nil, nil, 0)
	if len(c.handlers["pin.added"]) != 1 || len(c.handlers["job.queued"]) != 1 {
		t.Fatalf("handlers: %d for pins, %d for jobs", len(c.handlers["pin.added"]), len(c.handlers["job.queued"]))
	}
}

// A job that changed files says under its card how much, and offers the diff.
func TestJobCardCountsTheChanges(t *testing.T) {
	ctx := context.Background()
	task := db.Task{ID: newID(1), Title: "T", Kind: "bug", Phase: "implementation"}
	jobs := fakeJobs{fakeTasks: fakeTasks{task.ID.Bytes: task},
		jobs: map[[16]byte]db.Job{newID(5).Bytes: {ID: newID(5), TaskID: task.ID, Status: "running", Phase: "implementation"}},
		changes: map[[16]byte][][]byte{newID(5).Bytes: {[]byte(`[{"path":"a.go","added":40,"removed":7,"status":"modified"},` +
			`{"path":"b.go","added":2,"removed":0,"status":"added"}]`), // two repositories, counted together
			[]byte(`[{"path":"logo.png","added":0,"removed":0,"status":"added","binary":true}]`)}}}
	got, err := jobCards{jobs: jobs}.Cards(ctx, db.EventsAfterRow{AggregateID: newID(5)})
	if err != nil || len(got) != 1 {
		t.Fatalf("job card: %+v %v", got, err)
	}
	data := got[0].Card.Data
	changes, _ := data["changes"].(map[string]any)
	if changes["line"] != "3 files, +42 −7" || !slices.Contains(data["actions"].([]string), "diff") {
		t.Fatalf("card: %+v", data)
	}
	if !slices.Contains(jobCards{}.Events(), EventJobCheckpoint) {
		t.Fatal("the card does not follow checkpoints")
	}
}
