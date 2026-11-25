package e2e

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

func (h *harness) brainPreview(project gen.Project, task gen.Task) string {
	h.t.Helper()
	var pack gen.ContextPackage
	if code := h.do("GET", "/projects/"+project.Id.String()+"/brain/preview?role=worker&task="+task.Id.String(), nil, &pack); code != 200 {
		h.t.Fatalf("preview: %d", code)
	}
	var b strings.Builder
	for _, d := range pack.Documents {
		b.WriteString(d.Title + "\n" + deref(d.Body) + "\n")
	}
	return b.String()
}

// A person shares their context only where they belong: with a project they are in, or with the
// whole workspace once a workspace admin agrees. A job carries what its own project's people share,
// a few entries each and under their names, never a stranger's. Taking an entry back takes back
// its earlier versions too.
func TestAPersonSharesTheirContextOnlyWhereTheyBelong(t *testing.T) {
	h := newHarness(t)
	here, there := h.oneProject("share"), h.oneProject("elsewhere")
	ada, bob := h.member(here.Id.String(), "member"), h.member(here.Id.String(), "member")
	word := fmt.Sprintf("kafka%d", time.Now().UnixNano())
	share := func(token string, project *gen.Project, visibility, title string) (gen.BrainEntry, int) {
		v := gen.BrainVisibility(visibility)
		in := gen.NewBrainEntry{Scope: "user", Kind: "expertise", Title: title, Content: "Knows " + word + ".", Visibility: &v}
		if project != nil {
			in.ProjectId = &project.Id
		}
		var e gen.BrainEntry
		code := h.asJob(token, "POST", "/brain", in, &e)
		return e, code
	}
	if _, code := share(ada, &there, "project", "Elsewhere "+word); code != 403 {
		t.Fatalf("a person shares nothing with a project they are not in: %d", code)
	}
	mine, code := share(ada, &here, "project", "Streams "+word)
	if code != 201 || mine.Status != "approved" {
		t.Fatalf("with their own project they do: %d %+v", code, mine)
	}
	wide, _ := share(ada, nil, "workspace", "Everywhere "+word)
	if wide.Status != "proposed" {
		t.Fatalf("sharing with the whole workspace waits for an admin: %+v", wide)
	}
	if code := h.do("POST", "/brain/"+wide.Id.String()+"/approve", nil, nil); code != 200 {
		t.Fatalf("an admin approves it: %d", code)
	}
	for i := range 4 {
		share(ada, &here, "project", fmt.Sprintf("More %d %s", i, word))
	}

	inHere := h.brainPreview(here, h.taskIn(here.Id.String()))
	// h.member names a person after their role.
	if !strings.Contains(inHere, "member's expertise: Everywhere "+word) {
		t.Fatalf("a job of Ada's project carries what she shares, under her name:\n%s", inHere)
	}
	if n := strings.Count(inHere, "'s expertise: "); n != 3 {
		t.Fatalf("a few of her entries, not all six: %d\n%s", n, inHere)
	}
	if inThere := h.brainPreview(there, h.taskIn(there.Id.String())); strings.Contains(inThere, word) {
		t.Fatalf("a project Ada is not in carries nothing of hers, not even what she shares with everyone:\n%s", inThere)
	}

	// Taking it back.
	before := time.Now()
	time.Sleep(20 * time.Millisecond)
	private := gen.BrainVisibility("private")
	var v2 gen.BrainEntry
	if code := h.asJob(ada, "PATCH", "/brain/"+mine.Id.String(), gen.BrainPatch{Visibility: &private}, &v2); code != 200 || v2.Version != 2 {
		t.Fatalf("take it back: %d %+v", code, v2)
	}
	if code := h.asJob(bob, "GET", "/brain/"+mine.Id.String(), nil, nil); code != 404 {
		t.Fatalf("its earlier version is taken back too: %d", code)
	}
	for _, q := range []string{"status=superseded&q=" + word, "q=" + word + "&at=" + url.QueryEscape(before.UTC().Format(time.RFC3339Nano))} {
		if hitIDs(h.brainSearch(bob, q))[mine.Id.String()] {
			t.Fatalf("nor found in history (%s)", q)
		}
	}
}
