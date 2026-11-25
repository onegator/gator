package e2e

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
)

// text sends a GET with a token and answers the status and the body as text.
func (h *harness) text(token, path string) (int, string, http.Header) {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

const (
	commitA = "1111111111111111111111111111111111111111"
	commitB = "2222222222222222222222222222222222222222"
	commitC = "3333333333333333333333333333333333333333"
)

func turnDiff(path, removed, added string) string {
	return "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -1 +1 @@\n-" + removed + "\n+" + added + "\n"
}

// A runner's checkpoints are kept turn by turn, redacted, and read back by whoever may read the
// job: each turn's diff, the job's whole diff, a range of turns, without whitespace changes. The
// job's card says what it changed so far and offers the diff. A checkpoint of a job that only
// reads is refused, and nobody outside the project reads one.
func TestCheckpointsAreKeptAndReadBack(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	job := h.job(task, backend, "ok", 0)
	raw, _ := h.dialRaw(h.runner, backend, proto.Version, 1)
	if got := raw.lease(1); len(got) != 1 {
		t.Fatalf("lease: %+v", got)
	}
	id := job.Id.String()
	secret := "ghp_" + strings.Repeat("a", 36)
	first := proto.Checkpoint{JobID: id, Repo: "app", Seq: 1, BaseCommit: commitA, HeadCommit: commitB,
		Files:      []proto.FileChange{{Path: "config.go", Added: 1, Removed: 1, Status: proto.FileModified}},
		Diff:       []byte(turnDiff("config.go", "token := \"\"", "token := \""+secret+"\"")),
		WholeFiles: []proto.FileChange{{Path: "config.go", Added: 1, Removed: 1, Status: proto.FileModified}},
		Whole:      []byte(turnDiff("config.go", "token := \"\"", "token := \""+secret+"\""))}
	second := proto.Checkpoint{JobID: id, Repo: "app", Seq: 2, BaseCommit: commitB, HeadCommit: commitC,
		Files: []proto.FileChange{{Path: "main.go", Added: 1, Removed: 1, Status: proto.FileModified}},
		Diff:  []byte(turnDiff("main.go", "func  main() {}", "func main() {}")),
		WholeFiles: []proto.FileChange{{Path: "config.go", Added: 1, Removed: 1, Status: proto.FileModified},
			{Path: "main.go", Added: 1, Removed: 1, Status: proto.FileModified}},
		Whole: []byte(turnDiff("config.go", "a", "b") + turnDiff("main.go", "func  main() {}", "func main() {}"))}
	for _, cp := range []proto.Checkpoint{first, second, first} { // the last one a resend
		raw.send(proto.TypeCheckpoint, cp)
		raw.recvType(proto.TypeAck)
	}

	var cps []gen.Checkpoint
	if code := h.do("GET", "/jobs/"+id+"/checkpoints", nil, &cps); code != 200 || len(cps) != 2 {
		t.Fatalf("checkpoints: %d %+v", code, cps)
	}
	if cps[0].Seq != 1 || cps[1].Seq != 2 || cps[1].BaseCommit != commitB || len(cps[1].WholeFiles) != 2 || cps[0].Files[0].Path != "config.go" {
		t.Fatalf("checkpoints: %+v", cps)
	}
	code, diff, hdr := h.text(h.admin, "/checkpoints/"+cps[0].Id.String()+"/diff")
	if code != 200 || strings.Contains(diff, secret) || !strings.Contains(diff, "[redacted]") || hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("turn 1 diff (%d): %s", code, diff)
	}
	if _, whole, _ := h.text(h.admin, "/jobs/"+id+"/diff"); !strings.Contains(whole, "b/config.go") || !strings.Contains(whole, "b/main.go") {
		t.Fatalf("whole job: %s", whole)
	}
	if _, only, _ := h.text(h.admin, "/jobs/"+id+"/diff?from=2"); strings.Contains(only, "config.go") || !strings.Contains(only, "main.go") {
		t.Fatalf("turn 2 alone: %s", only)
	}
	if _, ws, _ := h.text(h.admin, "/checkpoints/"+cps[1].Id.String()+"/diff?ignoreWhitespace=true"); strings.Contains(ws, "main.go") {
		t.Fatalf("a whitespace-only change is left: %q", ws)
	}
	if code, _, _ := h.text(h.admin, "/jobs/"+id+"/diff?from=3&to=2"); code != 400 {
		t.Fatalf("from after to: %d", code)
	}

	// The card follows: two files, and the diff action.
	h.waitFor(t, "the job's card to count the changes", func() bool {
		var thread gen.ConversationDetail
		h.do("GET", "/tasks/"+task.Id.String()+"/conversation", nil, &thread)
		var page gen.MessagePage
		h.do("GET", "/conversations/"+thread.Id.String()+"/messages?limit=100", nil, &page)
		for _, m := range page.Messages {
			if m.Card != nil && m.CardType != nil && *m.CardType == "job" && strings.Contains(m.Body, "Job") {
				if c := m.Card.Changes; c != nil && c.Line == "2 files, +2 −2" {
					return true
				}
			}
		}
		return false
	})

	// Somebody outside the project reads none of it.
	outsider := h.person()
	if code, _, _ := h.text(outsider, "/jobs/"+id+"/checkpoints"); code != 404 && code != 403 {
		t.Fatalf("an outsider reads the checkpoints: %d", code)
	}
	if code, _, _ := h.text(outsider, "/checkpoints/"+cps[0].Id.String()+"/diff"); code != 404 && code != 403 {
		t.Fatalf("an outsider reads a diff: %d", code)
	}

	// A job that reads has a read-only checkout: a checkpoint of it is refused, and kept nowhere.
	if _, err := h.pool.Exec(context.Background(), "UPDATE jobs SET tier = 'chat' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	third := second
	third.Seq, third.BaseCommit, third.HeadCommit = 3, commitC, commitA
	raw.send(proto.TypeCheckpoint, third)
	if env := raw.recvType(proto.TypeError); env.Type != proto.TypeError {
		t.Fatalf("a chat job's checkpoint: %+v", env)
	}
	time.Sleep(50 * time.Millisecond)
	h.do("GET", "/jobs/"+id+"/checkpoints", nil, &cps)
	if len(cps) != 2 {
		t.Fatalf("a chat job's checkpoint was kept: %+v", cps)
	}
}
