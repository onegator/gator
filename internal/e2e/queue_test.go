package e2e

import (
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// While an agent answers, a message can wait for the turn's end instead of steering it: it shows in
// the conversation's queue, and its author can still send it at once, once.
func TestAMessageQueuedWhileAnAgentAnswers(t *testing.T) {
	h := newHarness(t)
	task := h.task()
	project := task.ProjectId.String()
	general := h.generalOf(project)
	root := h.say(project, general.Id.String(), "the totals are off")
	var thread gen.ConversationDetail
	h.do("GET", "/messages/"+root.Id.String()+"/thread", nil, &thread)
	ask := h.say(project, thread.Id.String(), "@reviewer look at it", "reviewer")
	h.turnOf(ask, project, "reviewer")

	body, queued := "then check the tests", gen.NewMessageIntent("queued")
	var msg gen.Message
	if code := h.do("POST", "/conversations/"+thread.Id.String()+"/messages", gen.NewMessage{Body: &body, Intent: &queued}, &msg); code != 201 ||
		msg.InputIntent == nil || *msg.InputIntent != "queued" {
		t.Fatalf("post: %d %+v", code, msg.InputIntent)
	}
	var queue []gen.Message
	if h.do("GET", "/conversations/"+thread.Id.String()+"/queue", nil, &queue); len(queue) != 1 || queue[0].Id != msg.Id {
		t.Fatalf("queue: %+v", queue)
	}
	if code := h.asJob(h.member(project, "member"), "POST", "/messages/"+msg.Id.String()+"/steer", nil, nil); code != 403 {
		t.Fatalf("somebody else sent it: %d", code)
	}
	var sent gen.Message
	if code := h.do("POST", "/messages/"+msg.Id.String()+"/steer", nil, &sent); code != 200 || *sent.InputIntent != "promoted_to_steer" {
		t.Fatalf("send now: %d %+v", code, sent.InputIntent)
	}
	if code := h.do("POST", "/messages/"+msg.Id.String()+"/steer", nil, nil); code != 409 {
		t.Fatalf("sent twice: %d", code)
	}
	if h.do("GET", "/conversations/"+thread.Id.String()+"/queue", nil, &queue); len(queue) != 0 {
		t.Fatalf("still queued: %+v", queue)
	}
}
