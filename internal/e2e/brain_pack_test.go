package e2e

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/server/api/gen"
)

var brainTag = regexp.MustCompile(`\[brain:[0-9a-f-]{36}\]`)

// citingExec answers by citing every Brain entry its package carried, and one it never had.
type citingExec struct {
	jobs  chan proto.Job
	extra string
}

func (e *citingExec) Run(_ context.Context, job proto.Job, _ client.JobIO) proto.Finish {
	e.jobs <- job
	var cites []string
	for _, d := range job.Context {
		cites = append(cites, brainTag.FindAllString(d.Title, -1)...)
	}
	summary := "I relied on " + strings.Join(cites, ", ") + " and on " + e.extra + "."
	return proto.Finish{Status: proto.StatusDone, Summary: summary, Usage: proto.Usage{Backend: job.Backend, DurationMS: 10, InputTokens: 1}}
}

// componentTask is a task about the auth component of a project where auth depends on tokens and
// billing stands apart.
func (h *harness) componentTask() (gen.Project, gen.Task) {
	h.t.Helper()
	p := h.oneProject("pack")
	h.putComponent(p.Id.String(), gen.ComponentInput{Key: "tokens", Name: "Tokens", Kind: gen.ComponentInputKindLib})
	h.putComponent(p.Id.String(), gen.ComponentInput{Key: "billing", Name: "Billing", Kind: gen.ComponentInputKindApi})
	h.putComponent(p.Id.String(), gen.ComponentInput{Key: "auth", Name: "Auth", Kind: gen.ComponentInputKindApi, DependsOn: &[]string{"tokens"}})
	task := h.taskIn(p.Id.String())
	key := "auth"
	if code := h.do("PUT", "/tasks/"+task.Id.String()+"/component", gen.TaskComponent{Key: &key}, nil); code != 200 {
		h.t.Fatalf("set component: %d", code)
	}
	return p, task
}

func (h *harness) componentEntry(p gen.Project, key, title, content string) gen.BrainEntry {
	h.t.Helper()
	in := gen.NewBrainEntry{Scope: "component", Kind: "decision", Title: title, Content: content, ProjectId: &p.Id, ComponentKey: &key}
	var e gen.BrainEntry
	if code := h.do("POST", "/brain", in, &e); code != 201 {
		h.t.Fatalf("component entry: %d", code)
	}
	return e
}

// A job about a component is handed the Brain's entries about it and its neighbours, and nothing
// about the rest of the product; what it was handed and what it cites of that is recorded, and
// what it cites without having been handed it is not.
func TestAJobIsHandedItsComponentsBrainAndCitesIt(t *testing.T) {
	h := newHarness(t)
	p, task := h.componentTask()
	own := h.componentEntry(p, "auth", "PASETO", "Auth uses PASETO v4, not JWT.")
	neighbour := h.componentEntry(p, "tokens", "Rotation", "Keys rotate weekly.")
	apart := h.componentEntry(p, "billing", "Invoices", "Invoices are immutable.")

	var preview gen.ContextPackage
	if code := h.do("GET", "/projects/"+p.Id.String()+"/brain/preview?role=worker&task="+task.Id.String(), nil, &preview); code != 200 {
		t.Fatalf("preview: %d", code)
	}
	titles := ""
	for _, d := range preview.Documents {
		titles += d.Title + "\n"
	}
	if !strings.Contains(titles, "[brain:"+own.Id.String()+"]") || !strings.Contains(titles, "[brain:"+neighbour.Id.String()+"]") {
		t.Fatalf("a job about auth reads auth's and its neighbour's entries:\n%s", titles)
	}
	if strings.Contains(titles, apart.Id.String()) {
		t.Fatalf("and nothing about billing:\n%s", titles)
	}
	if !strings.Contains(titles, "auth decision: PASETO (v1)") || strings.Index(titles, "PASETO") > strings.Index(titles, "Rotation") {
		t.Fatalf("its own component first:\n%s", titles)
	}

	backend := uniqueBackend()
	exec := &citingExec{jobs: make(chan proto.Job, 1), extra: "[brain:" + apart.Id.String() + "]"}
	h.startRunner(backend, exec)
	job := h.job(task, backend, "fix it", 0)
	<-exec.jobs
	h.waitJob(job.Id.String(), "done")

	uses := func(id, how string) int {
		return h.count(`SELECT count(*) FROM brain_citations WHERE job_id = $1 AND entry_id = $2 AND how = $3`, job.Id.String(), id, how)
	}
	for _, e := range []gen.BrainEntry{own, neighbour} {
		if uses(e.Id.String(), "packed") != 1 || uses(e.Id.String(), "cited") != 1 {
			t.Fatalf("%s was handed and cited: packed %d, cited %d", e.Title, uses(e.Id.String(), "packed"), uses(e.Id.String(), "cited"))
		}
	}
	if uses(apart.Id.String(), "cited") != 0 {
		t.Fatal("a job cannot cite what it was never handed")
	}
	var detail gen.BrainDetail
	if code := h.do("GET", "/brain/"+own.Id.String(), nil, &detail); code != 200 || len(detail.Citations) != 2 {
		t.Fatalf("an entry says where it was used: %d %+v", code, detail.Citations)
	}
}
