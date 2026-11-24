package agents

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

func cardData(t *testing.T, m db.Message) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(m.Card, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// "@worker fix it" where nobody allowed agents: @worker answers in chat and a card asks whether
// it may work. A viewer cannot say yes; a member can, which is the task's consent in their name,
// and @worker takes a work turn on the same question. Saying yes again changes nothing.
func TestTheConsentCardLetsAWorkAgentWork(t *testing.T) {
	w := newWorld(t)
	worker := w.agent("worker")
	ask := w.post(w.admin, w.general.ID, "@worker fix the flaky test", worker)
	chat := w.turnOf(worker, ask)
	card, err := w.q.FindCard(w.ctx, db.FindCardParams{CardKey: new("consent:" + uuidText(chat.ID)), ConversationIds: []pgtype.UUID{chat.ConversationID}})
	if err != nil {
		t.Fatalf("no consent card: %v", err)
	}
	if d := cardData(t, card); d["state"] != ConsentPending || d["title"] != "Let @worker work on this?" {
		t.Fatalf("card: %+v", d)
	}

	if _, err := w.mod.Consent.Allow(w.ctx, w.viewer, chat.ID); err != ErrForbidden {
		t.Fatalf("a viewer allowed agents: %v", err)
	}
	work, err := w.mod.Consent.Allow(w.ctx, w.admin, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if work.Tier != TierWork || work.EscalatedFrom != chat.ID || work.TriggerMessageID != card.ID || work.RequestedBy != w.admin.UserID ||
		!work.JobID.Valid || work.ChainID == chat.ChainID {
		t.Fatalf("work turn: %+v", work)
	}
	job, _ := w.q.GetJob(w.ctx, work.JobID)
	chatJob, _ := w.q.GetJob(w.ctx, chat.JobID)
	task, _ := w.q.GetTask(w.ctx, job.TaskID)
	if job.Tier != TierWork || job.TaskID != chatJob.TaskID || job.Instruction != chatJob.Instruction ||
		!task.AgentsAllowedAt.Valid || task.AgentsAllowedBy != w.admin.UserID {
		t.Fatalf("job %+v, task allowed %v by %v", job, task.AgentsAllowedAt.Valid, task.AgentsAllowedBy)
	}
	card, _ = w.q.GetMessage(w.ctx, card.ID)
	if d := cardData(t, card); d["state"] != ConsentAllowed {
		t.Fatalf("the card says it was allowed: %+v", d)
	}
	brief, err := w.mod.Briefer.Brief(w.ctx, job)
	if err != nil || brief.Ask == "" || !contains(brief.Repeat, "fix the flaky test") {
		t.Fatalf("the work turn answers the first question: %+v %v", brief, err)
	}

	again, err := w.mod.Consent.Allow(w.ctx, w.admin, chat.ID)
	if err != nil || again.ID != work.ID {
		t.Fatalf("allowing twice: %v %v", again.ID, err)
	}
	if n := w.count("SELECT count(*) FROM agent_turns WHERE escalated_from = $1", chat.ID); n != 1 {
		t.Fatalf("%d work turns", n)
	}

	// A chat agent never asks.
	reviewer := w.agent("reviewer")
	r := w.turnOf(reviewer, w.post(w.admin, w.general.ID, "@reviewer look", reviewer))
	if _, err := w.mod.Consent.Allow(w.ctx, w.admin, r.ID); err == nil {
		t.Fatal("a chat agent's turn was let work")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
