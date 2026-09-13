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
