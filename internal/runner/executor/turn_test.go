package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/workspace"
)

func replyJob(remote, tier string) proto.Job {
	return proto.Job{JobID: "job-reply-" + tier, TaskID: "task-1", Backend: "fake", Role: "reviewer", Phase: "answer",
		Kind: proto.KindReply, Tier: tier, Guide: "# Role: reviewer", TaskTitle: "Why is checkout slow?",
		Agent: &proto.Agent{Handle: "reviewer", DisplayName: "Reviewer"}, AgentToken: "gtr_job_x",
		Repo: &proto.Repo{Name: "app", URL: remote, DefaultBranch: "main"},
		Turn: &proto.Turn{ID: "turn-1", Ask: "Ada asks you, @reviewer, in #general:\n\nlook at PR #12",
			Conversation: []proto.ContextDoc{{Kind: "conversation", Title: "The conversation so far", Body: "Ada: it is slow"}},
			Repeat:       "Now answer Ada in the conversation: look at PR #12."}}
}

// A chat reply reads and answers: its agent gets tools that do not write and a checkout it
// cannot change, on no branch, so whatever it tries, nothing reaches the repository. Nobody is
// asked whether its turn may end, and the prompt reads ask first and ask last.
func TestAChatReplyCannotChangeTheRepository(t *testing.T) {
	remote := originRepo(t)
	before := git(t, remote, "rev-parse", "refs/heads/main")
	var tried error
	be := &scripted{run: func(_ context.Context, _ int, spec backend.Spec, _ backend.Emit) backend.Outcome {
		tried = os.WriteFile(filepath.Join(spec.Dir, "README"), []byte("changed\n"), 0o644)
		return backend.Outcome{Status: proto.StatusDone, SessionID: "s", Summary: "PR #12 looks fine.\n\n```gator-digest\n{\"changes\":[],\"decisions\":[],\"rejected\":[],\"left\":[]}\n```", Usage: usage()}
	}}
	e := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}, Push: true,
		ServerURL: "http://gator", CLIPath: "/opt/gator/gator-cli"}
	io, _, evs, _ := jobIO()
	asked := false
	io.TurnEnding = func(proto.TurnEnding) []proto.Objection { asked = true; return nil }
	fin := e.Run(context.Background(), replyJob(remote, proto.TierChat), io)

	if fin.Status != proto.StatusDone || fin.Branch != "" || len(fin.Commits) != 0 || fin.Summary != "PR #12 looks fine." || fin.Digest == nil {
		t.Fatalf("finish: %+v", fin)
	}
	if tried == nil {
		t.Fatal("the checkout of a chat job must not be writable")
	}
	spec := be.specs[0]
	if !spec.Access.ReadOnly || len(spec.Access.Commands) != 1 || spec.Access.Commands[0] != "gator-cli" {
		t.Fatalf("access: %+v", spec.Access)
	}
	for _, env := range spec.Env {
		if strings.HasPrefix(env, "GIT_CONFIG") {
			t.Fatalf("a read-only job signs no commits: %s", env)
		}
	}
	p := spec.Prompt
	if !strings.HasPrefix(p, "# Role: reviewer\n\n---\n\nAda asks you, @reviewer") || !strings.Contains(p, "## The conversation so far\n\nAda: it is slow") ||
		!strings.Contains(p, "read-only checkout") || !strings.Contains(p, "gator-cli say") || strings.Contains(p, "gator-cli artifact put") {
		t.Fatalf("prompt:\n%s", p)
	}
	if strings.Index(p, "Now answer Ada") < strings.Index(p, "The conversation so far") {
		t.Fatal("the ask is repeated after the conversation")
	}
	if asked {
		t.Fatal("a reply is nobody's to object to")
	}
	if after := git(t, remote, "rev-parse", "refs/heads/main"); after != before || strings.Contains(git(t, remote, "branch", "--list"), "gator/") {
		t.Fatal("a chat job pushed something")
	}
	if !strings.Contains(strings.Join(*evs, ","), "workspace") {
		t.Fatalf("events %v", *evs)
	}
}

// A work reply changes code on its branch, and every commit names the agent that made it.
func TestAWorkReplyCommitsSignedByItsAgent(t *testing.T) {
	remote := originRepo(t)
	be := &scripted{run: func(_ context.Context, n int, spec backend.Spec, _ backend.Emit) backend.Outcome {
		if n > 1 { // asked for the digest it forgot
			return backend.Outcome{Status: proto.StatusDone, SessionID: "s", Summary: "no digest", Usage: usage()}
		}
		_ = os.WriteFile(filepath.Join(spec.Dir, "fix.go"), []byte("package x\n"), 0o644)
		for _, args := range [][]string{{"add", "."}, {"-c", "user.name=a", "-c", "user.email=a@a", "commit", "-q", "-m", "Fix the slow query"}} {
			c := exec.Command("git", args...)
			c.Dir, c.Env = spec.Dir, append(os.Environ(), spec.Env...)
			if out, err := c.CombinedOutput(); err != nil {
				t.Errorf("git %v: %v %s", args, err, out)
			}
		}
		return backend.Outcome{Status: proto.StatusDone, SessionID: "s", Summary: "Fixed.", Usage: usage()}
	}}
	e := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}, Push: true}
	io, _, _, _ := jobIO()
	job := replyJob(remote, proto.TierWork)
	job.Agent = &proto.Agent{Handle: "worker", DisplayName: "Worker"}
	fin := e.Run(context.Background(), job, io)
	if fin.Status != proto.StatusDone || len(fin.Commits) != 1 || !strings.HasPrefix(fin.Branch, "gator/") {
		t.Fatalf("finish: %+v", fin)
	}
	if be.specs[0].Access.ReadOnly {
		t.Fatal("a work job has every tool")
	}
	msg := git(t, remote, "log", "-1", "--format=%B", "refs/heads/"+fin.Branch)
	if strings.Count(msg, "Co-Authored-By: @worker via Gator") != 1 {
		t.Fatalf("the commit names its agent once:\n%s", msg)
	}
}

// A summary is all answer: it gets the summarizer's prompt and its text whole, and no digest is
// asked for.
func TestASummaryJobIsJustTheSummary(t *testing.T) {
	be := &scripted{run: func(_ context.Context, _ int, spec backend.Spec, _ backend.Emit) backend.Outcome {
		return backend.Outcome{Status: proto.StatusDone, SessionID: "s", Summary: "They agreed to bill monthly.", Usage: usage()}
	}}
	e := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be},
		ServerURL: "http://gator", CLIPath: "/opt/gator/gator-cli"}
	io, _, evs, _ := jobIO()
	fin := e.Run(context.Background(), proto.Job{JobID: "sum", Backend: "fake", Kind: proto.KindSummary, Tier: proto.TierChat,
		Guide: "# Role: summarizer", Instruction: "Ada: bill monthly?\nBo: yes", AgentToken: "gtr_job_leaked",
		Repo: &proto.Repo{Name: "app", URL: originRepo(t), DefaultBranch: "main"}}, io)
	spec := be.specs[0]
	if !spec.Access.NoTools || len(spec.Env) != 0 {
		t.Fatalf("a summary has no tools and no identity: %+v %v", spec.Access, spec.Env)
	}
	if entries, _ := os.ReadDir(spec.Dir); len(entries) != 0 {
		t.Fatalf("a summary gets no checkout: %v", entries)
	}
	if fin.Status != proto.StatusDone || fin.Summary != "They agreed to bill monthly." || fin.Digest != nil || len(be.specs) != 1 {
		t.Fatalf("finish %+v after %d runs", fin, len(be.specs))
	}
	if p := be.specs[0].Prompt; !strings.HasPrefix(p, "# Role: summarizer") || !strings.Contains(p, "Bo: yes") || strings.Contains(p, "gator-digest") {
		t.Fatalf("prompt:\n%s", p)
	}
	if strings.Contains(strings.Join(*evs, ","), "digest_requested") {
		t.Fatal("no digest is asked of a summary")
	}
}

// A backend that cannot hold an agent to read-only never runs a chat job.
func TestAChatJobNeedsABackendThatKeepsLimits(t *testing.T) {
	be := &scripted{noLimits: true, run: func(context.Context, int, backend.Spec, backend.Emit) backend.Outcome {
		t.Fatal("a backend without limits ran a chat job")
		return backend.Outcome{}
	}}
	e := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}}
	io, _, _, _ := jobIO()
	fin := e.Run(context.Background(), replyJob(originRepo(t), proto.TierChat), io)
	if fin.Status != proto.StatusFailed || !strings.Contains(fin.StopReason, "cannot hold") {
		t.Fatalf("finish: %+v", fin)
	}
}
