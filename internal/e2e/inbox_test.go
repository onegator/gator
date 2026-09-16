package e2e

import (
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

func (h *harness) decisions(query string) []gen.Decision {
	h.t.Helper()
	var out []gen.Decision
	if code := h.do("GET", "/inbox"+query, nil, &out); code != 200 {
		h.t.Fatalf("inbox%s: %d", query, code)
	}
	return out
}

func has(decisions []gen.Decision, task gen.Task) *gen.Decision {
	for i := range decisions {
		if decisions[i].Task.Id == task.Id {
			return &decisions[i]
		}
	}
	return nil
}

// The inbox can be narrowed to the tasks a person holds, and each row says how often the
// task came back to its phase.
func TestInboxMineAndRollbackCount(t *testing.T) {
	h := newHarness(t)
	mine, other := h.task(), h.task()
	var me gen.Me
	if code := h.do("GET", "/auth/me", nil, &me); code != 200 || me.UserId == nil {
		t.Fatalf("me: %d %+v", code, me)
	}
	if code := h.do("POST", "/tasks/"+mine.Id.String()+"/handoff", gen.Handoff{ToKind: gen.HandoffToKindUser, ToId: *me.UserId}, nil); code != 200 {
		t.Fatalf("handoff: %d", code)
	}

	all := h.decisions("")
	if has(all, mine) == nil || has(all, other) == nil {
		t.Fatal("both tasks wait in the full inbox")
	}
	onlyMine := h.decisions("?mine=true")
	if has(onlyMine, mine) == nil || has(onlyMine, other) != nil {
		t.Fatalf("mine should hold only the task handed to me: %d rows", len(onlyMine))
	}

	// A rollback into the current phase is counted on the row. Advancing hands the task to
	// the next phase's runner, so it leaves "mine" on the way.
	if d := has(all, mine); d.Rollbacks != nil && *d.Rollbacks != 0 {
		t.Fatalf("a fresh task has no rollbacks: %+v", d.Rollbacks)
	}
	advanced := h.approveAdvance(mine)
	if code := h.do("POST", "/tasks/"+advanced.Id.String()+"/rollback", gen.Rollback{To: "planning", Reason: "the plan misses the offline case"}, nil); code != 200 {
		t.Fatalf("rollback: %d", code)
	}
	d := has(h.decisions(""), mine)
	if d == nil || d.Rollbacks == nil || *d.Rollbacks != 1 {
		t.Fatalf("one rollback into planning: %+v", d)
	}
	if d.Reason != gen.DecisionReasonApproval {
		t.Fatalf("a task sent back waits for a person again: %s", d.Reason)
	}
	// The earlier approval of that phase must not carry over.
	var detail gen.TaskDetail
	h.do("GET", "/tasks/"+mine.Id.String(), nil, &detail)
	if detail.Gate.HumanApproved || len(detail.Gate.Checks) != 0 {
		t.Fatalf("the gate must start over: %+v", detail.Gate)
	}
}
