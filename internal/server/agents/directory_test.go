package agents

import (
	"errors"
	"testing"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

// Deleting an agent puts it away: not while it is answering, and never its turns, which are the
// record of what it did and what it cost. Its handle is free again.
func TestDeletingAnAgentKeepsItsTurns(t *testing.T) {
	w := newWorld(t)
	a, err := w.mod.Directory.Create(w.ctx, w.admin, w.project, Draft{Handle: "helper-" + unique()[:8], DisplayName: "Helper"})
	if err != nil {
		t.Fatal(err)
	}
	msg := w.post(w.admin, w.general.ID, "@helper look", a)
	turn := w.turnOf(a, msg)
	if err := w.mod.Directory.Delete(w.ctx, w.admin, a.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("an agent still answering was put away: %v", err)
	}
	if _, err := w.q.SetTurnStatus(w.ctx, db.SetTurnStatusParams{ID: turn.ID, Status: TurnDone}); err != nil {
		t.Fatal(err)
	}
	if err := w.mod.Directory.Delete(w.ctx, w.admin, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.q.GetTurn(w.ctx, turn.ID); err != nil {
		t.Fatalf("its turn is gone: %v", err)
	}
	if _, err := w.mod.Directory.Get(w.ctx, w.admin, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a put-away agent is still there: %v", err)
	}
	if _, err := w.chat.Messages.Post(w.ctx, w.admin, w.general.ID, conversations.Draft{Body: "@helper again", Mentions: mentioning(a)}); !errors.Is(err, conversations.ErrInvalid) {
		t.Fatalf("a put-away agent was mentioned: %v", err)
	}
	if _, err := w.mod.Directory.Create(w.ctx, w.admin, w.project, Draft{Handle: a.Handle, DisplayName: "Helper 2"}); err != nil {
		t.Fatalf("the handle is free again: %v", err)
	}
	if _, err := w.pool.Exec(w.ctx, "DELETE FROM agents WHERE id = $1", a.ID); err == nil {
		t.Fatal("an agent with turns was erased")
	}
}
