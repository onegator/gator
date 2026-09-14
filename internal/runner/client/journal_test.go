package client

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/onegator/gator/internal/proto"
)

func TestJournalSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.json")
	c1 := New(Config{JournalPath: path}, Unconfigured{}, slog.Default())
	c1.sendReliable(&outMsg{typ: proto.TypeEvents, payload: proto.Events{JobID: "j1", Events: []proto.JobEvent{{Seq: 1, Type: "text"}}}})
	c1.sendReliable(&outMsg{typ: proto.TypeFinish, payload: proto.Finish{JobID: "j1", Status: proto.StatusDone}, jobID: "j1"})

	c2 := New(Config{JournalPath: path}, Unconfigured{}, slog.Default())
	c2.restore()
	if len(c2.pending) != 2 || c2.pending[1].typ != proto.TypeFinish {
		t.Fatalf("restored %d messages", len(c2.pending))
	}
	if active := c2.ActiveJobs(); len(active) != 1 || active[0] != "j1" {
		t.Fatalf("a restored finish must keep its job in the heartbeat: %v", active)
	}

	// acking drops the entry from disk
	c2.mu.Lock()
	c2.inflight[7] = c2.pending[1]
	c2.mu.Unlock()
	c2.ack(7)
	c3 := New(Config{JournalPath: path}, Unconfigured{}, slog.Default())
	c3.restore()
	if len(c3.pending) != 1 || c3.pending[0].typ != proto.TypeEvents {
		t.Fatalf("after ack the journal should hold only the event, got %d", len(c3.pending))
	}
	if len(c3.ActiveJobs()) != 0 {
		t.Fatal("acked finish should release the job")
	}
}
