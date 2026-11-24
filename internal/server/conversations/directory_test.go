package conversations

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
)

// The rules of Access hold against the real store: a viewer reads a channel but cannot open
// one, a private channel is invisible to the project outside it, and a DM outside any project is
// its people's alone.
func TestDirectoryKeepsConversationsToTheirReaders(t *testing.T) {
	f := newFixture(t)
	admin, member, viewer, stranger := f.person("admin"), f.person("member"), f.person("viewer"), f.person("stranger")
	project := f.project(map[auth.Principal]string{admin: "admin", member: "member", viewer: "viewer"})

	if _, err := f.directory.CreateChannel(f.ctx, viewer, project, NewChannel{Name: "ideas"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a viewer opened a channel: %v", err)
	}
	open, err := f.directory.CreateChannel(f.ctx, member, project, NewChannel{Name: "#Backend", Topic: "APIs"})
	if err != nil {
		t.Fatal(err)
	}
	if *open.Name != "backend" {
		t.Fatalf("name: %q", *open.Name)
	}
	if _, err := f.directory.CreateChannel(f.ctx, admin, project, NewChannel{Name: "backend"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second #backend: %v", err)
	}
	secret, err := f.directory.CreateChannel(f.ctx, member, project, NewChannel{Name: "hiring", Visibility: VisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		who  auth.Principal
		conv pgtype.UUID
		want Rights
	}{
		{"viewer reads the open channel", viewer, open.ID, Rights{Read: true, React: true}},
		{"stranger cannot read the open channel", stranger, open.ID, Rights{}},
		{"its creator owns the private channel", member, secret.ID, Rights{Read: true, React: true, Write: true, Moderate: true}},
		{"the project's admin is not in the private channel", admin, secret.ID, Rights{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got, err := f.access.RightsByID(f.ctx, tc.conv, tc.who)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("rights = %+v, want %+v", got, tc.want)
			}
		})
	}

	names := func(who auth.Principal) map[string]bool {
		t.Helper()
		ls, err := f.directory.ListProject(f.ctx, who, project, KindChannel, false)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, l := range ls {
			out[*l.Conversation.Name] = true
		}
		return out
	}
	if got := names(admin); !got["backend"] || got["hiring"] {
		t.Fatalf("the admin lists %v", got)
	}
	if got := names(member); !got["backend"] || !got["hiring"] {
		t.Fatalf("the private channel's owner lists %v", got)
	}
	if _, err := f.directory.Get(f.ctx, admin, secret.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the admin got the private channel: %v", err)
	}

	// Adding the admin lets them in; a viewer of the project may be added but not write.
	if _, err := f.directory.SetMember(f.ctx, member, secret.ID, Member{Kind: MemberUser, ID: admin.UserID}, MemberChange{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.directory.Get(f.ctx, admin, secret.ID); err != nil {
		t.Fatalf("the admin, once added: %v", err)
	}
	if _, err := f.directory.SetMember(f.ctx, member, secret.ID, Member{Kind: MemberUser, ID: stranger.UserID}, MemberChange{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("somebody outside the project was added: %v", err)
	}
	if _, err := f.directory.SetMember(f.ctx, admin, secret.ID, Member{Kind: MemberUser, ID: member.UserID}, MemberChange{Notify: ptr(NotifyNone)}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("somebody chose how another hears about things: %v", err)
	}

	// A DM outside any project: opening it again answers the same one, in any order.
	dm, created, err := f.directory.OpenDM(f.ctx, member, []pgtype.UUID{stranger.UserID}, pgtype.UUID{})
	if err != nil || !created {
		t.Fatalf("open DM: %v %v", created, err)
	}
	again, created, err := f.directory.OpenDM(f.ctx, stranger, []pgtype.UUID{member.UserID, stranger.UserID}, pgtype.UUID{})
	if err != nil || created || again.ID != dm.ID {
		t.Fatalf("the same people opened another DM: %v %v", created, err)
	}
	if _, err := f.directory.Get(f.ctx, admin, dm.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone outside a DM read it: %v", err)
	}
	if _, err := f.directory.Get(f.ctx, stranger, dm.ID); err != nil {
		t.Fatalf("a person in the DM: %v", err)
	}
	if err := f.directory.RemoveMember(f.ctx, stranger, dm.ID, Member{Kind: MemberUser, ID: stranger.UserID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("somebody left a DM: %v", err)
	}
	// Inside a project, a DM is only for its people.
	if _, _, err := f.directory.OpenDM(f.ctx, member, []pgtype.UUID{stranger.UserID}, project); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a DM in a project with somebody outside it: %v", err)
	}
	mine, err := f.directory.ListMine(f.ctx, stranger, KindDM)
	if err != nil || len(mine) != 1 || mine[0].Conversation.ID != dm.ID {
		t.Fatalf("the stranger's DMs: %+v %v", mine, err)
	}
}
