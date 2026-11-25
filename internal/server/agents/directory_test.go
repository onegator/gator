package agents

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

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

// A workspace agent works in every project, so what it spent is for workspace admins to read.
func TestAWorkspaceAgentsRecordIsForAdmins(t *testing.T) {
	w := newWorld(t)
	a, err := w.q.CreateAgent(w.ctx, db.CreateAgentParams{Handle: "ws-" + unique()[:10], DisplayName: "WS", Tier: TierChat,
		Policy: []byte("{}"), Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = w.pool.Exec(w.ctx, "UPDATE agents SET deleted_at = now() WHERE id = $1", a.ID) })
	turn := w.turnOf(a, w.post(w.admin, w.general.ID, "@ws look", a))
	w.spend(turn.JobID, 3)
	_, st, err := w.mod.Directory.Stats(w.ctx, w.admin, a.ID)
	if err != nil || st.CostUSD != 0 || st.Turns != 0 {
		t.Fatalf("a project member read a workspace agent's record: %+v %v", st, err)
	}
	boss := w.admin
	boss.WorkspaceRole = "admin"
	if _, st, _ := w.mod.Directory.Stats(w.ctx, boss, a.ID); st.CostUSD < 3 || st.Turns != 1 {
		t.Fatalf("a workspace admin reads it: %+v", st)
	}
}

// What an agent says is cut on a character, never inside one.
func TestALongSayIsCutCleanly(t *testing.T) {
	w := newWorld(t)
	planner := w.agent("planner")
	turn := w.turnOf(planner, w.post(w.admin, w.general.ID, "@planner plan", planner))
	job, _ := w.q.GetJob(w.ctx, turn.JobID)
	msg, err := w.mod.Talk.Say(w.ctx, job, "x"+strings.Repeat("ż", maxSay))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(msg.Body) || !strings.HasSuffix(msg.Body, "…") {
		t.Fatalf("cut badly: %q", msg.Body[len(msg.Body)-8:])
	}
}
