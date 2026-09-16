package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/notify"
	"github.com/onegator/gator/internal/server/store/db"
)

// fakeSender stands in for Apple.
type fakeSender struct {
	mu   sync.Mutex
	sent []notify.Message
}

func (f *fakeSender) Name() string { return "fake" }

func (f *fakeSender) Send(_ context.Context, _ db.Device, m notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeSender) messages() []notify.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.Message(nil), f.sent...)
}

// A blocked gate reaches the person's device, and a forgotten device stops receiving.
func TestDevicesAndNotifications(t *testing.T) {
	h := newHarness(t)
	token := fmt.Sprintf("apns-%d", time.Now().UnixNano())
	var device gen.Device
	if code := h.do("POST", "/devices", gen.NewDevice{Token: token, Platform: gen.Macos}, &device); code != 200 {
		t.Fatalf("register: %d", code)
	}

	sender := &fakeSender{}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go notify.New(h.pool, h.svc, sender, h.hub, slog.Default(), 50*time.Millisecond).Run(ctx)
	time.Sleep(150 * time.Millisecond) // let the cursor start at the newest event

	task := h.task()
	if code := h.do("PUT", "/tasks/"+task.Id.String()+"/checks",
		gen.Check{Name: "ci", Source: "plugin:test", Status: gen.Fail, Detail: ptr("the build is red")}, nil); code != 200 {
		t.Fatalf("set check: %d", code)
	}

	var got notify.Message
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range sender.messages() {
			if m.Link == "gator://tasks/"+task.Id.String() {
				got = m
			}
		}
		if got.Link != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.Link == "" {
		t.Fatalf("no notification for the blocked task; sent: %+v", sender.messages())
	}
	if got.Title != task.Title || got.Body == "" {
		t.Fatalf("message: %+v", got)
	}

	if code := h.do("DELETE", "/devices/"+token, nil, nil); code != 204 {
		t.Fatalf("forget: %d", code)
	}
	if code := h.do("DELETE", "/devices/"+token, nil, nil); code != 404 {
		t.Fatalf("forgetting twice: %d", code)
	}
}

func ptr[T any](v T) *T { return &v }
