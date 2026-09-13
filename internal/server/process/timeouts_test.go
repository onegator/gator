package process

import (
	"context"
	"testing"
	"time"
)

func TestExpirePhasesBlocksOverdueTasks(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	projectID, userID := newProject(t, pool)
	svc := service(t, pool)
	user := Actor{Kind: ActorUser, ID: userID}

	// incident/implementation has a 6h timeout; chore/implementation 24h
	inc, err := svc.Create(ctx, CreateParams{ProjectID: projectID, Kind: "incident", Title: "down"}, user)
	if err != nil {
		t.Fatal(err)
	}
	chore, err := svc.Create(ctx, CreateParams{ProjectID: projectID, Kind: "chore", Title: "deps"}, user)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE tasks SET phase_entered_at = now() - interval '7 hours' WHERE id IN ($1,$2)", inc.ID, chore.ID); err != nil {
		t.Fatal(err)
	}
	blocked, err := svc.ExpirePhases(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 || blocked[0] != uuidString(inc.ID) {
		t.Fatalf("expected only the incident to expire, got %v", blocked)
	}
	d, _ := svc.Detail(ctx, inc.ID)
	if d.Task.BlockedReason == nil || d.Gate.BlockedBy == nil || *d.Gate.BlockedBy != "automation" {
		t.Fatalf("incident not blocked by automation: %+v", d.Task.BlockedReason)
	}
	// second pass is idempotent
	if again, _ := svc.ExpirePhases(ctx, time.Now()); len(again) != 0 {
		t.Fatalf("second pass blocked %v", again)
	}
	// unblock lets it move again
	if err := svc.Unblock(ctx, inc.ID, user, "looked at it"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Approve(ctx, inc.ID, userID); err != nil {
		t.Fatal(err)
	}
	if task, err := svc.Advance(ctx, inc.ID, user, ""); err != nil || task.Phase != "verification" {
		t.Fatalf("after unblock: %q %v", task.Phase, err)
	}
}
