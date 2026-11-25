package e2e

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/notify"
	"github.com/onegator/gator/internal/server/store/db"
)

// pushes records what would have gone to Apple.
type pushes struct {
	mu  sync.Mutex
	got []notify.Message
}

func (p *pushes) Send(_ context.Context, _ db.Device, m notify.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, m)
	return nil
}

func (p *pushes) Name() string { return "test" }

// A tool approval waiting for the task's owner reaches their phone, with the buttons' category and
// the request they act on.
func TestAToolApprovalIsPushed(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	me := h.userIDOf(h.admin)
	if _, err := h.pool.Exec(context.Background(), "UPDATE tasks SET owner_kind = 'user', owner_id = $2 WHERE id = $1", task.Id.String(), me); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(context.Background(), "INSERT INTO devices (user_id, token, platform) VALUES ($1, 'tok-1', 'ios')", me); err != nil {
		t.Fatal(err)
	}
	sent := &pushes{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notify.New(h.pool, h.svc, sent, h.hub, slog.Default(), 50*time.Millisecond).WithAgents().Run(ctx)

	job := h.job(task, backend, "ok", 0)
	raw, _ := h.dialRaw(h.runner, backend, proto.Version, 1)
	raw.lease(1)
	raw.send(proto.TypeRuntimeRequest, proto.RuntimeRequest{ID: "66666666-6666-4666-8666-666666666666", JobID: job.Id.String(),
		Kind: proto.RequestToolApproval, Tool: "Bash", Input: []byte(`{"command":"git push --force"}`), Rule: "Bash(git push:*)", TimeoutSeconds: 600})
	raw.recvType(proto.TypeAck)
	h.waitFor(t, "the push", func() bool {
		sent.mu.Lock()
		defer sent.mu.Unlock()
		for _, m := range sent.got {
			if m.Category == notify.CategoryToolApproval && m.Data["request_id"] == "66666666-6666-4666-8666-666666666666" {
				return true
			}
		}
		return false
	})
}
