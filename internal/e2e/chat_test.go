//go:build unix

package e2e

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/backend/claude"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/runner/executor"
	"github.com/onegator/gator/internal/runner/workspace"
	"github.com/onegator/gator/internal/server/api/gen"
)

// repoProject is a project with a repository a runner can clone; it answers the project and the
// bare origin, which a test can ask what was pushed.
func (h *harness) repoProject() (gen.Project, string) {
	h.t.Helper()
	var p gen.Project
	if code := h.do("POST", "/projects", gen.NewProject{Slug: "chat" + uniqueBackend()[5:], Name: "Chat"}, &p); code != 201 {
		h.t.Fatalf("project: %d", code)
	}
	root := h.t.TempDir()
	src := filepath.Join(root, "src")
	gitCmd(h.t, root, "init", "-q", "-b", "main", src)
	_ = os.WriteFile(filepath.Join(src, "README"), []byte("app\n"), 0o644)
	gitCmd(h.t, src, "add", ".")
	gitCmd(h.t, src, "commit", "-q", "-m", "init")
	origin := filepath.Join(root, "origin.git")
	gitCmd(h.t, root, "clone", "-q", "--bare", src, origin)
	def, primary := "main", true
	if code := h.do("PUT", "/projects/"+p.Id.String()+"/repos", gen.ProjectRepo{Name: "app", Url: origin, DefaultBranch: &def, Primary: &primary}, nil); code != 200 {
		h.t.Fatalf("add repo: %d", code)
	}
	return p, origin
}

// claudeRunner is a runner for one project with the real executor and a recorded Claude Code.
func (h *harness) claudeRunner(project, fixture string) string {
	h.t.Helper()
	wd, _ := os.Getwd()
	td := filepath.Join(wd, "..", "runner", "backend", "claude", "testdata")
	args := filepath.Join(h.t.TempDir(), "args")
	h.t.Setenv("FAKE_CLAUDE_FIXTURE", filepath.Join(td, fixture))
	h.t.Setenv("FAKE_CLAUDE_ARGS", args)
	h.t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fake-token-for-tests")
	h.t.Setenv("FAKE_CLAUDE_RESUME_FIXTURE", filepath.Join(td, "synthetic-digest-reply.ndjson"))
	ex := &executor.Executor{
		WS:       &workspace.Manager{Root: h.t.TempDir()},
		Backends: map[string]backend.Backend{"claude": claude.Backend{Bin: filepath.Join(td, "fake-claude.sh"), AllowEnv: []string{"FAKE_CLAUDE_*"}}},
		Push:     true,
	}
	c := client.New(client.Config{ServerURL: h.srv.URL, Token: h.runner, Name: "e2e-chat", Location: "other",
		Backends: []string{"claude"}, Projects: []string{project}, MaxParallel: 1,
		HeartbeatInterval: 100 * time.Millisecond, MinBackoff: 50 * time.Millisecond}, ex, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	go func() { _ = c.Run(ctx) }()
	return args
}

// message reads one message as the harness's person.
func (h *harness) message(id string) gen.Message {
	h.t.Helper()
	var m gen.Message
	if code := h.do("GET", "/messages/"+id, nil, &m); code != 200 {
		h.t.Fatalf("message %s: %d", id, code)
	}
	return m
}

// settledMessage waits for a message to be what ok wants.
func (h *harness) settledMessage(id string, ok func(gen.Message) bool) gen.Message {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		m := h.message(id)
		if ok(m) {
			return m
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("message %s stayed %s: %q (turn %+v)", id, m.Status, m.Body, m.Turn)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// "@reviewer look at PR #12" in #general: the reviewer answers in the thread, in chat, read-only,
// with what it cost under the answer, and nothing reaches the repository — even an agent that
// tries to write (this recording tries Write and a touch; the fake tries to commit).
func TestAMentionInGeneralIsAnsweredInChat(t *testing.T) {
	h := newHarness(t)
	p, origin := h.repoProject()
	project := p.Id.String()
	args := h.claudeRunner(project, "claude-2.1.331-chat-readonly.ndjson")
	t.Setenv("FAKE_CLAUDE_COMMIT", "1")

	general := h.generalOf(project)
	ask := h.say(project, general.Id.String(), "@reviewer look at PR #12", "reviewer")
	turn := h.turnOf(ask, project, "reviewer")
	if turn.Tier != "chat" || turn.Reply == nil {
		t.Fatalf("turn: %+v", turn)
	}
	reply := h.settledMessage(*turn.Reply, func(m gen.Message) bool { return m.Status == "final" })
	if !strings.Contains(reply.Body, "Write tool attempt") || strings.Contains(reply.Body, "gator-digest") ||
		reply.AuthorKind != "agent" || reply.ConversationId.String() != turn.Conversation {
		t.Fatalf("the answer: %+v", reply)
	}
	if reply.Turn == nil || reply.Turn.Status != "done" || reply.Turn.Tier != "chat" || reply.Turn.CostUsd < 0.018 || reply.Turn.DurationMs <= 0 {
		t.Fatalf("what is said under the answer: %+v", reply.Turn)
	}
	var thread gen.ConversationDetail
	h.do("GET", "/messages/"+ask.Id.String()+"/thread", nil, &thread)
	if thread.Id.String() != turn.Conversation {
		t.Fatal("the answer is in the thread under the mention")
	}

	job := h.waitJob(*turn.Job, "done")
	if r := *job.Receipt; r["branch"] != nil || r["commits"] != nil {
		t.Fatalf("a chat job leaves no branch: %+v", r)
	}
	if branches := gitCmd(t, origin, "branch", "--list"); strings.Contains(branches, "gator/") {
		t.Fatalf("something reached the repository: %s", branches)
	}
	called, _ := os.ReadFile(args)
	if !strings.Contains(string(called), "--permission-mode\ndontAsk\n") || strings.Contains(string(called), "bypassPermissions") {
		t.Fatalf("the session ran without tools that write:\n%s", called)
	}
	// The job's card follows it in the thread, signed by the reviewer.
	eventually(t, "the job's card says whose work it is", func() bool {
		var page gen.MessagePage
		h.do("GET", "/conversations/"+turn.Conversation+"/messages?afterSeq=0", nil, &page)
		for _, m := range page.Messages {
			if m.CardType != nil && *m.CardType == "job" && m.Card != nil && deref(m.Card.Title) == "@reviewer" && deref(m.Card.State) == "done" {
				return true
			}
		}
		return false
	})
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
