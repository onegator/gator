package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/runner/workspace"
)

// commitAs commits everything in dir as the agent would.
func commitAs(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "."}, {"-c", "commit.gpgsign=false", "commit", "-q", "-m", msg}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@a", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@a")
		if out, err := c.CombinedOutput(); err != nil {
			t.Errorf("git %v: %v %s", args, err, out)
		}
	}
}

// On a real repository: a checkpoint after each turn (the first, and the one a steer resumed),
// each with only its own turn's changes, sent before the receipt; the branch that is pushed holds
// the agent's commits and none of the runner's, and checkpoint refs never leave the runner.
func TestCheckpointAfterEveryTurnOnARealRepository(t *testing.T) {
	remote := originRepo(t)
	be := &scripted{run: func(ctx context.Context, n int, spec backend.Spec, _ backend.Emit) backend.Outcome {
		if n == 1 {
			_ = os.WriteFile(filepath.Join(spec.Dir, "api.go"), []byte("package api\n\nfunc Serve() {}\n"), 0o644)
			commitAs(t, spec.Dir, "api")
			<-ctx.Done() // a person steers
			return backend.Outcome{Interrupted: true, Cause: context.Cause(ctx), SessionID: "s", Usage: usage()}
		}
		// The second turn leaves its change uncommitted: the checkpoint has it all the same.
		_ = os.WriteFile(filepath.Join(spec.Dir, "README"), []byte("hi\nonly the API layer\n"), 0o644)
		return backend.Outcome{Status: proto.StatusDone, Summary: "done" + digestBlock, SessionID: "s", Usage: usage()}
	}}
	ex := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}, Push: true}
	var mu sync.Mutex
	var sent []proto.Checkpoint
	var evs []string
	steer := make(chan string, 1)
	io := client.JobIO{Steer: steer,
		Emit: func(typ string, _ any) { mu.Lock(); evs = append(evs, typ); mu.Unlock() },
		Checkpoint: func(cp proto.Checkpoint) {
			mu.Lock()
			sent = append(sent, cp)
			evs = append(evs, "(sent)")
			mu.Unlock()
		}}
	go func() { time.Sleep(200 * time.Millisecond); steer <- "only the API layer" }()
	job := proto.Job{JobID: "job-cp-1", TaskID: "task-cp-1", Backend: "fake", Repo: &proto.Repo{Name: "app", URL: remote, DefaultBranch: "main"}}
	fin := ex.Run(context.Background(), job, io)

	if fin.Status != proto.StatusDone || len(sent) != 2 || len(fin.Checkpoints) != 2 {
		t.Fatalf("finish %+v, %d checkpoints sent", fin, len(sent))
	}
	first, second := sent[0], sent[1]
	if first.Seq != 1 || second.Seq != 2 || first.Repo != "app" || second.BaseCommit != first.HeadCommit {
		t.Fatalf("checkpoints: %+v / %+v", first, second)
	}
	if len(first.Files) != 1 || first.Files[0].Path != "api.go" || !strings.Contains(string(first.Diff), "+func Serve() {}") {
		t.Fatalf("first turn: %+v\n%s", first.Files, first.Diff)
	}
	if len(second.Files) != 1 || second.Files[0].Path != "README" || strings.Contains(string(second.Diff), "Serve") {
		t.Fatalf("second turn: %+v\n%s", second.Files, second.Diff)
	}
	if len(second.WholeFiles) != 2 {
		t.Fatalf("the job so far: %+v", second.WholeFiles)
	}
	if fin.Checkpoints[0].Added != 3 || fin.Checkpoints[1].Files != 1 {
		t.Fatalf("receipt: %+v", fin.Checkpoints)
	}
	// The pushed branch is the agent's commit on main; no checkpoint, no hidden ref, went out.
	if len(fin.Commits) != 1 {
		t.Fatalf("commits: %+v", fin.Commits)
	}
	log := git(t, remote, "log", "--format=%s", "refs/heads/"+fin.Branch)
	if log != "api\ninit" {
		t.Fatalf("pushed branch: %q", log)
	}
	if refs := git(t, remote, "for-each-ref", "refs/gator/"); refs != "" {
		t.Fatalf("checkpoint refs reached the remote: %q", refs)
	}
	joined := strings.Join(evs, ",")
	if !strings.Contains(joined, "(sent),checkpoint") || strings.Count(joined, "checkpoint") != 2 {
		t.Fatalf("events: %s", joined)
	}
}

// A chat job's checkout is read-only, and a project may turn checkpoints off: neither takes any.
func TestNoCheckpointsForChatOrWhenTurnedOff(t *testing.T) {
	remote := originRepo(t)
	be := &scripted{run: func(context.Context, int, backend.Spec, backend.Emit) backend.Outcome {
		return backend.Outcome{Status: proto.StatusDone, Summary: "ok" + digestBlock, Usage: usage()}
	}}
	for name, job := range map[string]proto.Job{
		"chat": {JobID: "job-cp-chat", TaskID: "t", Backend: "fake", Tier: proto.TierChat, Kind: proto.KindReply,
			Repo: &proto.Repo{Name: "app", URL: remote, DefaultBranch: "main"}},
		"off": {JobID: "job-cp-off", TaskID: "t", Backend: "fake", NoCheckpoints: true,
			Repo: &proto.Repo{Name: "app", URL: remote, DefaultBranch: "main"}},
	} {
		ex := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}}
		n := 0
		io := client.JobIO{Emit: func(string, any) {}, Steer: make(chan string), Checkpoint: func(proto.Checkpoint) { n++ }}
		if fin := ex.Run(context.Background(), job, io); fin.Status != proto.StatusDone || n != 0 || len(fin.Checkpoints) != 0 {
			t.Fatalf("%s: %+v, %d checkpoints", name, fin, n)
		}
	}
}
