package e2e

import (
	"context"
	"testing"

	"github.com/onegator/gator/internal/server/api/gen"
)

// Forgetting is not undone behind a person's back: after DELETE /me/brain their focus is not
// worked out again by the hourly sweep, GET /me/brain says it is off, and it comes back only when
// they turn it on.
func TestForgettingStaysForgotten(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.oneProject("forget")
	ada := h.member(p.Id.String(), "member")
	task := h.taskIn(p.Id.String())
	var thread gen.ConversationDetail
	h.do("GET", "/tasks/"+task.Id.String()+"/conversation", nil, &thread)
	line := "on it"
	h.asJob(ada, "POST", "/conversations/"+thread.Id.String()+"/messages", gen.NewMessage{Body: &line}, nil)
	user := h.userIDOf(ada)
	mine := func() gen.MyBrain {
		var out gen.MyBrain
		h.asJob(ada, "GET", "/me/brain", nil, &out)
		return out
	}
	if err := h.brain.Focus.Refresh(ctx, user); err != nil {
		t.Fatal(err)
	}
	if b := mine(); len(b.Entries) != 1 || !b.Focus.Enabled {
		t.Fatalf("Ada's focus is worked out: %+v", b)
	}
	if code := h.asJob(ada, "DELETE", "/me/brain", nil, nil); code != 200 {
		t.Fatalf("forget: %d", code)
	}
	if err := h.brain.Focus.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.brain.Focus.Refresh(ctx, user); err != nil {
		t.Fatal(err)
	}
	if b := mine(); len(b.Entries) != 0 || b.Focus.Enabled || b.Focus.OffSince == nil {
		t.Fatalf("forgotten stays forgotten, and says so: %+v", b)
	}
	var on gen.FocusSetting
	if code := h.asJob(ada, "PUT", "/me/brain/focus", gen.FocusSetting{Enabled: true}, &on); code != 200 || !on.Enabled {
		t.Fatalf("turn it on: %d %+v", code, on)
	}
	if err := h.brain.Focus.Refresh(ctx, user); err != nil {
		t.Fatal(err)
	}
	if b := mine(); len(b.Entries) != 1 {
		t.Fatalf("and it is worked out again: %+v", b.Entries)
	}
}
