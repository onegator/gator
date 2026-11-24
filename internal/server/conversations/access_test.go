package conversations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

func TestDecide(t *testing.T) {
	channel := scope{kind: KindChannel, hasProject: true}
	private := scope{kind: KindChannel, hasProject: true, private: true}
	archived := scope{kind: KindChannel, hasProject: true, archived: true}
	dmOutside := scope{kind: KindDM, private: true}
	dmInside := scope{kind: KindDM, private: true, hasProject: true}
	task := scope{kind: KindTask, hasProject: true}

	none := Rights{}
	readReact := Rights{Read: true, React: true}
	writer := Rights{Read: true, React: true, Write: true}
	moderator := Rights{Read: true, React: true, Write: true, Moderate: true}

	for _, tc := range []struct {
		name  string
		scope scope
		st    standing
		want  Rights
	}{
		{"stranger to the project reads no channel", channel, standing{}, none},
		{"viewer reads and reacts but does not write", channel, standing{role: auth.RoleViewer}, readReact},
		{"member writes", channel, standing{role: auth.RoleMember}, writer},
		{"project admin moderates", channel, standing{role: auth.RoleAdmin}, moderator},
		{"owner of a channel moderates it", channel, standing{role: auth.RoleMember, member: true, owner: true}, moderator},
		{"task thread follows the project", task, standing{role: auth.RoleViewer}, readReact},

		{"private: project member outside it reads nothing", private, standing{role: auth.RoleMember}, none},
		{"private: project admin outside it reads nothing", private, standing{role: auth.RoleAdmin}, none},
		{"private: member who left the project reads nothing", private, standing{member: true}, none},
		{"private: viewer inside reads", private, standing{role: auth.RoleViewer, member: true}, readReact},
		{"private: member inside writes", private, standing{role: auth.RoleMember, member: true}, writer},

		{"archived: nobody writes, nobody reacts", archived, standing{role: auth.RoleAdmin}, Rights{Read: true, Moderate: true}},

		{"DM outside a project: its people write", dmOutside, standing{member: true}, writer},
		{"DM outside a project: nobody else reads", dmOutside, standing{role: auth.RoleAdmin}, none},
		{"DM in a project: its people read only while in the project", dmInside, standing{member: true}, none},
		{"DM in a project: a viewer in it answers", dmInside, standing{member: true, role: auth.RoleViewer}, writer},
		{"DM in a project: admin in it does not moderate", dmInside, standing{member: true, role: auth.RoleAdmin}, writer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decide(tc.scope, tc.st); got != tc.want {
				t.Fatalf("decide = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// fakeRoles answers project roles from a table keyed by user.
type fakeRoles map[[16]byte]auth.Role

func (f fakeRoles) ProjectRole(_ context.Context, p auth.Principal, _ pgtype.UUID) (auth.Role, error) {
	if p.IsWorkspaceAdmin() {
		return auth.RoleAdmin, nil
	}
	return f[p.UserID.Bytes], nil
}

// fakeStore holds conversations, the messages threads hang under, and memberships.
type fakeStore struct {
	conversations map[[16]byte]db.Conversation
	messageConv   map[[16]byte]pgtype.UUID // message -> conversation
	members       map[[2][16]byte]db.ConversationMember
	users         map[[16]byte]db.User
}

func (f *fakeStore) GetConversation(_ context.Context, id pgtype.UUID) (db.Conversation, error) {
	c, ok := f.conversations[id.Bytes]
	if !ok {
		return c, pgx.ErrNoRows
	}
	return c, nil
}

func (f *fakeStore) GetAccessRoot(ctx context.Context, id pgtype.UUID) (db.Conversation, error) {
	c, err := f.GetConversation(ctx, id)
	if err != nil || !c.ParentMessageID.Valid {
		return c, err
	}
	return f.GetConversation(ctx, f.messageConv[c.ParentMessageID.Bytes])
}

func (f *fakeStore) GetConversationMember(_ context.Context, arg db.GetConversationMemberParams) (db.ConversationMember, error) {
	m, ok := f.members[[2][16]byte{arg.ConversationID.Bytes, arg.MemberID.Bytes}]
	if !ok {
		return m, pgx.ErrNoRows
	}
	return m, nil
}

func (f *fakeStore) GetUser(_ context.Context, id pgtype.UUID) (db.User, error) {
	u, ok := f.users[id.Bytes]
	if !ok {
		return u, pgx.ErrNoRows
	}
	return u, nil
}

func newID(n byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{n, 1}, Valid: true} }

func user(id pgtype.UUID) auth.Principal {
	return auth.Principal{Kind: auth.KindUser, UserID: id, WorkspaceRole: "member"}
}

// Access finds the facts decide needs: a thread is judged by the conversation its root message
// is in, membership is looked up there, and an archived thread stops writing on its own.
func TestAccessJudgesAThreadByItsRoot(t *testing.T) {
	project := newID(1)
	alice, bob, carol := newID(10), newID(11), newID(12)
	secret := db.Conversation{ID: newID(20), ProjectID: project, Kind: KindChannel, Visibility: VisibilityPrivate}
	rootMsg := newID(30)
	thread := db.Conversation{ID: newID(21), ProjectID: project, Kind: KindThread, Visibility: VisibilityProject, ParentMessageID: rootMsg}
	closed := thread
	closed.ID, closed.ArchivedAt = newID(22), pgtype.Timestamptz{Time: time.Now(), Valid: true}
	st := &fakeStore{
		conversations: map[[16]byte]db.Conversation{secret.ID.Bytes: secret, thread.ID.Bytes: thread, closed.ID.Bytes: closed},
		messageConv:   map[[16]byte]pgtype.UUID{rootMsg.Bytes: secret.ID},
		members: map[[2][16]byte]db.ConversationMember{
			{secret.ID.Bytes, alice.Bytes}: {ConversationID: secret.ID, MemberKind: MemberUser, MemberID: alice, Role: RoleMember},
		},
		users: map[[16]byte]db.User{carol.Bytes: {ID: carol, WorkspaceRole: "member"}},
	}
	a := NewAccess(fakeRoles{alice.Bytes: auth.RoleMember, bob.Bytes: auth.RoleMember, carol.Bytes: auth.RoleMember}, st)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		conv db.Conversation
		who  auth.Principal
		want Rights
	}{
		{"member of the private channel writes in its thread", thread, user(alice), Rights{Read: true, React: true, Write: true}},
		{"project member outside the channel cannot read its thread", thread, user(bob), Rights{}},
		{"an archived thread is read but not written", closed, user(alice), Rights{Read: true}},
		{"a runner is in no private channel", thread, auth.Principal{Kind: auth.KindRunner}, Rights{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.Rights(ctx, tc.conv, tc.who)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("rights = %+v, want %+v", got, tc.want)
			}
		})
	}

	if _, r, err := a.RightsByID(ctx, newID(99), user(alice)); err != nil || r.Read {
		t.Fatalf("a conversation that does not exist: rights %+v, err %v", r, err)
	}
	if ok, err := a.UserCanRead(ctx, thread, carol); err != nil || ok {
		t.Fatalf("carol is not in the private channel, yet may read its thread: %v %v", ok, err)
	}
	if ok, err := a.UserCanRead(ctx, thread, newID(77)); err != nil || ok {
		t.Fatalf("somebody unknown may read: %v %v", ok, err)
	}
}

func TestDMKeyIgnoresOrder(t *testing.T) {
	a, b, c := newID(1), newID(2), newID(3)
	if dmKey([]pgtype.UUID{a, b, c}) != dmKey([]pgtype.UUID{c, a, b}) {
		t.Fatal("the same people make a different DM in another order")
	}
	if dmKey([]pgtype.UUID{a, b}) == dmKey([]pgtype.UUID{a, c}) {
		t.Fatal("different people share a DM")
	}
}

func TestNormalizeChannelName(t *testing.T) {
	for in, want := range map[string]string{"#General": "general", " backend-api ": "backend-api", "ops_2": "ops_2"} {
		if got, err := normalizeChannelName(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "#", "-dash", "has space", "zażółć", string(make([]byte, 81))} {
		if _, err := normalizeChannelName(in); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
}

func TestTaskChannelName(t *testing.T) {
	for cfg, want := range map[string]string{
		`{"conversations":{"task_channel":"#Work"}}`:  "work",
		`{"conversations":{}}`:                        "",
		`{"conversations":{"task_channel":"no way"}}`: "",
		`{}`:     "",
		``:       "",
		`broken`: "",
	} {
		if got := taskChannelName([]byte(cfg)); got != want {
			t.Errorf("%s: %q, want %q", cfg, got, want)
		}
	}
}

func TestFoundHidesTheStore(t *testing.T) {
	if !errors.Is(found(pgx.ErrNoRows), ErrNotFound) || errors.Is(found(pgx.ErrNoRows), pgx.ErrNoRows) {
		t.Fatal("no rows does not leave as ErrNotFound")
	}
	other := errors.New("db down")
	if found(other) != other || found(nil) != nil {
		t.Fatal("found changed another error")
	}
}
