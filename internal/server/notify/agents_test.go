package notify

import (
	"strings"
	"testing"

	"github.com/onegator/gator/internal/server/store/db"
)

// A request waits for the person the turn acts for, else the task's owner; a task nobody owns
// tells nobody rather than everyone.
func TestWhoARequestWaitsFor(t *testing.T) {
	user := "user"
	task := db.Task{OwnerKind: &user, OwnerID: id(2)}
	if got := whoWaits(&db.AgentTurn{OnBehalfOf: id(1)}, task); got != id(1) {
		t.Fatalf("on behalf of: %v", got)
	}
	if got := whoWaits(&db.AgentTurn{}, task); got != id(2) {
		t.Fatalf("the owner: %v", got)
	}
	if got := whoWaits(nil, db.Task{}); got.Valid {
		t.Fatalf("nobody owns it: %v", got)
	}
}

// The notification of a tool approval has its buttons' category and what they act on; a
// question has its own.
func TestWaitingMessage(t *testing.T) {
	bash := "Bash"
	req := db.RuntimeRequest{ID: id(7), Kind: "tool_approval", Tool: &bash, Input: []byte(`{"command":"git   push --force"}`)}
	m := waitingMessage(req, "@worker", db.Task{ID: id(3), Title: "Fix login"})
	if m.Title != "@worker asks to use Bash" || m.Category != CategoryToolApproval || m.Data["request_id"] == "" ||
		!strings.HasPrefix(m.Body, "git push --force") || !strings.HasPrefix(m.Link, "gator://runtime-requests/") {
		t.Fatalf("message: %+v", m)
	}
	req.Kind = "question"
	if m := waitingMessage(req, "@worker", db.Task{}); m.Category != CategoryQuestion || m.Title != "@worker asks you a question" {
		t.Fatalf("question: %+v", m)
	}
}
