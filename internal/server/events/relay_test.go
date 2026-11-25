package events

import (
	"encoding/json"
	"testing"
	"time"
)

func TestHubRoutesByTopic(t *testing.T) {
	h := NewHub()
	inbox, stopInbox := h.Subscribe("inbox")
	defer stopInbox()
	task, stopTask := h.Subscribe("task:abc")
	defer stopTask()
	other, stopOther := h.Subscribe("task:zzz")
	defer stopOther()

	h.Publish(Event{ID: 1, Type: "task.phase_changed", Aggregate: "task", AggregateID: "abc", Payload: json.RawMessage(`{}`)})

	for name, ch := range map[string]<-chan Event{"inbox": inbox, "task": task} {
		select {
		case e := <-ch:
			if e.ID != 1 {
				t.Fatalf("%s got %d", name, e.ID)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not receive", name)
		}
	}
	select {
	case e := <-other:
		t.Fatalf("unrelated topic received %v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHubRoutesAProjectsEventsToItsTopic(t *testing.T) {
	h := NewHub()
	mine, stopMine := h.Subscribe("project:p1")
	defer stopMine()
	theirs, stopTheirs := h.Subscribe("project:p2")
	defer stopTheirs()

	h.Publish(Event{ID: 7, Type: "task.closed", Aggregate: "task", AggregateID: "abc", ProjectID: "p1", Payload: json.RawMessage(`{}`)})

	select {
	case e := <-mine:
		if e.ID != 7 {
			t.Fatalf("got %d", e.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("the project's topic did not receive its task's event")
	}
	select {
	case e := <-theirs:
		t.Fatalf("another project's topic received %v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

// A conversation's events go to its own topic and to each person in their audience, and to
// nobody else's.
func TestHubRoutesAConversationsEventsToItsTopicAndItsAudience(t *testing.T) {
	h := NewHub()
	conv, stopConv := h.Subscribe("conversation:c1")
	defer stopConv()
	alice, stopAlice := h.Subscribe("user:alice")
	defer stopAlice()
	bob, stopBob := h.Subscribe("user:bob")
	defer stopBob()

	h.Publish(Event{ID: 9, Type: "message.posted", Aggregate: "conversation", AggregateID: "c1",
		Payload: json.RawMessage(`{}`), Audience: []string{"alice"}})

	for name, ch := range map[string]<-chan Event{"conversation": conv, "alice": alice} {
		select {
		case e := <-ch:
			if e.ID != 9 {
				t.Fatalf("%s got %d", name, e.ID)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not receive", name)
		}
	}
	select {
	case e := <-bob:
		t.Fatalf("somebody outside the audience received %v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

// A personal event reaches its audience alone, and PublishTo the topics named alone: neither
// wakes whoever follows the firehose or the conversation.
func TestHubKeepsPersonalAndDirectEventsToTheirTopics(t *testing.T) {
	h := NewHub()
	all, stopAll := h.Subscribe("*")
	defer stopAll()
	conv, stopConv := h.Subscribe("conversation:c1")
	defer stopConv()
	alice, stopAlice := h.Subscribe("user:alice")
	defer stopAlice()

	h.Publish(Event{ID: 1, Type: "conversation.read", Aggregate: "conversation", AggregateID: "c1", ProjectID: "p1",
		Payload: json.RawMessage(`{}`), Audience: []string{"alice"}, Personal: true})
	select {
	case e := <-alice:
		if e.ID != 1 {
			t.Fatalf("got %d", e.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("the reader's own topic did not receive it")
	}
	h.PublishTo(Event{ID: 2, Type: "conversation.typing", Aggregate: "conversation", AggregateID: "c1"}, "conversation:c1")
	select {
	case e := <-conv:
		if e.ID != 2 {
			t.Fatalf("the conversation heard %d", e.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("the conversation did not hear the typing")
	}
	select {
	case e := <-all:
		t.Fatalf("the firehose heard %v", e)
	case <-alice:
		t.Fatal("typing reached a personal topic")
	case <-time.After(50 * time.Millisecond):
	}
}
