package approvals

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/store/db"
)

// What each decision leaves a request in; a question is answered with its answer, never allowed
// for a session, and nothing else is a decision.
func TestStatusFor(t *testing.T) {
	for _, c := range []struct {
		kind, decision, feedback, want string
		invalid                        bool
	}{
		{proto.RequestToolApproval, DecideAllow, "", StatusAllowed, false},
		{proto.RequestToolApproval, DecideAllowForSession, "", StatusAllowedForSession, false},
		{proto.RequestToolApproval, DecideDeny, "no", StatusDenied, false},
		{proto.RequestToolApproval, DecideStop, "", StatusDenied, false},
		{proto.RequestQuestion, DecideAllow, "Postgres", StatusAllowed, false},
		{proto.RequestQuestion, DecideAllow, "", "", true},
		{proto.RequestQuestion, DecideAllowForSession, "x", "", true},
		{proto.RequestToolApproval, "yes", "", "", true},
	} {
		got, err := statusFor(c.kind, c.decision, c.feedback)
		if c.invalid != (err != nil) || (err != nil && !errors.Is(err, ErrInvalid)) || got != c.want {
			t.Errorf("%s %s %q: %q %v", c.kind, c.decision, c.feedback, got, err)
		}
	}
}

// A card says who asks to use what, with the buttons while it waits and who decided once decided.
func TestCard(t *testing.T) {
	bash := "Bash"
	r := db.RuntimeRequest{ID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, Kind: proto.RequestToolApproval, Tool: &bash,
		Input: []byte(`{"command":"curl  https://x"}`), Rule: "Bash(curl:*)", Status: StatusPending,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
	agent := db.Agent{Handle: "worker"}
	c := Card(r, db.Job{}, &agent, "")
	if c.Type != CardToolApproval || c.Key != "approval:"+uuidString(r.ID) || c.Data["title"] != "@worker asks to use Bash" ||
		c.Data["summary"] != "curl https://x" || len(c.Data["actions"].([]string)) != 4 {
		t.Fatalf("waiting card: %+v", c)
	}
	no := "use the vendored copy"
	r.Status, r.Feedback = StatusDenied, &no
	c = Card(r, db.Job{}, &agent, "Ada")
	if len(c.Data["actions"].([]string)) != 0 || !strings.Contains(c.Body, "denied by Ada") || c.Data["feedback"] != no {
		t.Fatalf("decided card: %+v", c)
	}
	r.Stopped = true
	if c := Card(r, db.Job{}, &agent, "Ada"); !strings.Contains(c.Body, "the turn was stopped by Ada") {
		t.Fatalf("stopped: %s", c.Body)
	}
	q := db.RuntimeRequest{Kind: proto.RequestQuestion, Input: []byte(`{"questions":[{"question":"Which database?"}]}`), Status: StatusPending}
	if c := Card(q, db.Job{}, nil, ""); c.Type != CardQuestion || c.Data["summary"] != "Which database?" ||
		strings.Join(c.Data["actions"].([]string), ",") != "answer,stop" {
		t.Fatalf("question card: %+v", c)
	}
	big := db.RuntimeRequest{Kind: proto.RequestToolApproval, Input: []byte(`{"command":"` + strings.Repeat("x", 3000) + `"}`)}
	if in, ok := Card(big, db.Job{}, nil, "").Data["input"].(map[string]any); !ok || in["note"] == nil {
		t.Fatalf("a long input on a card: %v", in)
	}
}

// What the runner hears of a decided request.
func TestDecisionOf(t *testing.T) {
	for status, want := range map[string]string{StatusAllowed: proto.DecisionAllow, StatusAllowedForSession: proto.DecisionAllowForSession,
		StatusDenied: proto.DecisionDeny, StatusExpired: proto.DecisionExpired, StatusCancelled: proto.DecisionCancelled} {
		if got := decisionOf(t.Context(), nil, db.RuntimeRequest{Status: status}); got.Decision != want {
			t.Errorf("%s: %s", status, got.Decision)
		}
	}
	if got := decisionOf(t.Context(), nil, db.RuntimeRequest{Status: StatusDenied, Stopped: true}); got.Decision != proto.DecisionStop {
		t.Errorf("stopped: %s", got.Decision)
	}
}
