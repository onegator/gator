package bridges

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/server/words"
	"github.com/onegator/gator/plugin"
)

// ActionApprove approves what a decision card waits for: the same as the card's button in Gator
// (POST /tasks/{id}/approve).
const ActionApprove = "approve"

// cardActions are the buttons a bridge may offer on a card. Approving is the one a person is asked
// for where they are; the others (rollback needs a phase and a reason, consent for an agent to work
// is given in Gator) stay in Gator, and the card links there.
var cardActions = []string{ActionApprove}

// decisionData is what a decision card says of itself.
type decisionData struct {
	State   string   `json:"state"`
	Actions []string `json:"actions"`
	Ref     struct {
		TaskID string `json:"taskId"`
	} `json:"ref"`
}

// offered are the actions of a card a bridge may show as buttons.
func offered(m db.Message) []string {
	if m.Kind != "card" || m.CardType == nil || *m.CardType != conversations.CardDecision || m.DeletedAt.Valid {
		return nil
	}
	var d decisionData
	if json.Unmarshal(m.Card, &d) != nil || d.State != "pending" {
		return nil
	}
	var out []string
	for _, a := range d.Actions {
		if slices.Contains(cardActions, a) {
			out = append(out, a)
		}
	}
	return out
}

// CardAction is card.action: a button pressed outside on a card the bridge mirrored, done for the
// person the outside account is linked to, with their role in the project. An account linked to
// nobody, or to somebody who could not press it in Gator, presses nothing.
func (s *Service) CardAction(ctx context.Context, b Bridge, p plugin.CardActionParams) (plugin.CardActionResult, error) {
	id, ok := words.UUID(p.MessageID)
	if !ok {
		return plugin.CardActionResult{}, invalid("message_id %q is not a uuid", p.MessageID)
	}
	msg, err := s.q.GetMessage(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return plugin.CardActionResult{}, notFound("message not found")
	}
	if err != nil {
		return plugin.CardActionResult{}, err
	}
	if _, err := s.bridged(ctx, b, msg.ConversationID); err != nil {
		return plugin.CardActionResult{}, err
	}
	// Only a card this bridge was shown: the buttons it may have drawn are the ones mirrored.
	if _, err := s.q.GetMessageRefOf(ctx, db.GetMessageRefOfParams{Provider: b.Provider, MessageID: msg.ID}); errors.Is(err, pgx.ErrNoRows) {
		return plugin.CardActionResult{}, notFound("this card was not mirrored through this bridge")
	} else if err != nil {
		return plugin.CardActionResult{}, err
	}
	if !slices.Contains(offered(msg), p.Action) {
		return plugin.CardActionResult{Message: "This card has nothing to " + p.Action + " any more."}, nil
	}
	who, err := s.identity(ctx, b, p.Actor)
	if err != nil {
		return plugin.CardActionResult{}, err
	}
	if !who.UserID.Valid {
		return plugin.CardActionResult{Message: "Only a person with a Gator account linked here can do that. Link yours with a code from Gator."}, nil
	}
	u, err := s.q.GetUser(ctx, who.UserID)
	if err != nil {
		return plugin.CardActionResult{}, err
	}
	role, err := s.deps.Roles.ProjectRole(ctx, principalOf(u), b.Project)
	if err != nil {
		return plugin.CardActionResult{}, err
	}
	if role < auth.RoleMember {
		return plugin.CardActionResult{Message: "Approving needs a member of the project; your Gator account is not one."}, nil
	}
	var d decisionData
	_ = json.Unmarshal(msg.Card, &d)
	task, ok := words.UUID(d.Ref.TaskID)
	if !ok {
		return plugin.CardActionResult{}, invalid("the card names no task")
	}
	t, err := s.q.GetTask(ctx, task)
	if err != nil || t.ProjectID != b.Project {
		return plugin.CardActionResult{}, notFound("task not found in this project")
	}
	// The card is one wait: a phase entered at one moment (its key). Approval holds only while the
	// task still waits there, checked under the task's lock: a task that moved on waits on a gate
	// nobody outside was shown, whatever an old button says.
	phase, entered, ok := waitOf(msg)
	if !ok {
		return plugin.CardActionResult{}, invalid("the card names no wait")
	}
	err = s.deps.Approver.ApproveWaiting(ctx, task, u.ID, phase, entered)
	switch {
	case errors.Is(err, process.ErrTaskClosed):
		return plugin.CardActionResult{Message: "The task is closed."}, nil
	case errors.Is(err, process.ErrGateMoved):
		return plugin.CardActionResult{Message: "The task has moved on since this card; decide in Gator."}, nil
	case err != nil:
		return plugin.CardActionResult{}, err
	}
	return plugin.CardActionResult{Done: true, Message: "Approved by " + u.Name + "."}, nil
}

// waitOf reads a decision card's key, decision:<phase>:<unix micro of entering it>.
func waitOf(m db.Message) (string, time.Time, bool) {
	if m.CardKey == nil {
		return "", time.Time{}, false
	}
	rest, ok := strings.CutPrefix(*m.CardKey, "decision:")
	i := strings.LastIndex(rest, ":")
	if !ok || i <= 0 {
		return "", time.Time{}, false
	}
	micro, err := strconv.ParseInt(rest[i+1:], 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return rest[:i], time.UnixMicro(micro), true
}
