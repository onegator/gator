package conversations

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// Rights is what one caller may do in one conversation.
type Rights struct {
	// Read is seeing the conversation, its members and its messages.
	Read bool
	// React is adding and taking back reactions. A viewer reacts but does not write.
	React bool
	// Write is posting, replying, uploading, changing the topic and adding people.
	Write bool
	// Moderate is renaming and archiving, removing others and deleting their messages.
	Moderate bool
}

// RoleResolver answers a caller's role in a project. auth.Authorizer is one.
type RoleResolver interface {
	ProjectRole(ctx context.Context, p auth.Principal, project pgtype.UUID) (auth.Role, error)
}

// accessStore is what Access reads. *db.Queries is one.
type accessStore interface {
	GetConversation(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
	GetAccessRoot(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
	GetConversationMember(ctx context.Context, arg db.GetConversationMemberParams) (db.ConversationMember, error)
	GetUser(ctx context.Context, id pgtype.UUID) (db.User, error)
}

// Access decides who may do what in a conversation. It is the one place the rules live: the
// API, the WebSocket and search all ask it.
//
//   - A project channel (and a task thread) is read by everyone who may read the project, and
//     written by its members and admins; a viewer reads and reacts but does not write.
//   - A private channel is read only by its members, who must still be able to read the project.
//   - A DM is read only by its people; a DM inside a project also needs the project.
//   - A thread is read by whoever may read the conversation its root message lives in.
//   - Nobody writes in an archived conversation.
//   - Project admins and a conversation's owners moderate it; a DM has nobody above its people.
//
// Workspace admins are admins of every project, so they moderate its channels, but they read a
// private channel or a DM only when they are in it.
type Access struct {
	roles RoleResolver
	store accessStore
}

// NewAccess builds the rules on top of a project role resolver and the conversation store.
func NewAccess(roles RoleResolver, store accessStore) *Access {
	return &Access{roles: roles, store: store}
}

// scope is the part of a conversation the rules look at: that of its access root.
type scope struct {
	kind       string
	private    bool
	hasProject bool
	archived   bool
}

// standing is the caller's place in it.
type standing struct {
	role   auth.Role
	member bool
	owner  bool
}

// decide is the rules themselves, apart from how the facts are found.
func decide(s scope, st standing) Rights {
	var read bool
	switch {
	case s.kind == KindDM:
		read = st.member && (!s.hasProject || st.role >= auth.RoleViewer)
	case s.private:
		read = st.member && st.role >= auth.RoleViewer
	default:
		read = s.hasProject && st.role >= auth.RoleViewer
	}
	if !read {
		return Rights{}
	}
	write := !s.archived && (!s.hasProject || st.role >= auth.RoleMember)
	moderate := st.owner || (s.kind != KindDM && s.hasProject && st.role >= auth.RoleAdmin)
	return Rights{Read: true, React: !s.archived, Write: write, Moderate: moderate}
}

// Rights answers what p may do in conv.
func (a *Access) Rights(ctx context.Context, conv db.Conversation, p auth.Principal) (Rights, error) {
	root, err := a.root(ctx, conv)
	if err != nil {
		return Rights{}, err
	}
	st, err := a.standing(ctx, root, p)
	if err != nil {
		return Rights{}, err
	}
	return decide(scopeOf(root, conv), st), nil
}

// RightsByID is Rights for a conversation known by its id. A conversation that does not exist
// has no rights in it, and no error: the caller answers both the same way.
func (a *Access) RightsByID(ctx context.Context, id pgtype.UUID, p auth.Principal) (db.Conversation, Rights, error) {
	conv, err := a.store.GetConversation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Conversation{}, Rights{}, nil
	}
	if err != nil {
		return db.Conversation{}, Rights{}, err
	}
	r, err := a.Rights(ctx, conv, p)
	return conv, r, err
}

// CanRead answers whether p may read conv.
func (a *Access) CanRead(ctx context.Context, conv db.Conversation, p auth.Principal) (bool, error) {
	r, err := a.Rights(ctx, conv, p)
	return r.Read, err
}

// OpenToProject answers whether everyone who may read conv's project may read conv: a project
// channel or a thread under one, not a private channel or a DM. What is said in a conversation
// that is not open to the project stays in it.
func (a *Access) OpenToProject(ctx context.Context, conv db.Conversation) (bool, error) {
	root, err := a.root(ctx, conv)
	if err != nil {
		return false, err
	}
	return openToProject(root), nil
}

func openToProject(root db.Conversation) bool {
	return root.Kind != KindDM && root.Visibility == VisibilityProject && root.ProjectID.Valid
}

// UserCanRead answers whether a person, who is not the caller, may read conv: whether they may
// be mentioned in it or told about it.
func (a *Access) UserCanRead(ctx context.Context, conv db.Conversation, user pgtype.UUID) (bool, error) {
	u, err := a.store.GetUser(ctx, user)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return a.CanRead(ctx, conv, principalOf(u))
}

// principalOf is the identity a person would sign in as.
func principalOf(u db.User) auth.Principal {
	return auth.Principal{Kind: auth.KindUser, UserID: u.ID, WorkspaceRole: u.WorkspaceRole, Name: u.Name}
}

func (a *Access) root(ctx context.Context, conv db.Conversation) (db.Conversation, error) {
	if !conv.ParentMessageID.Valid {
		return conv, nil
	}
	return a.store.GetAccessRoot(ctx, conv.ID)
}

func (a *Access) standing(ctx context.Context, root db.Conversation, p auth.Principal) (standing, error) {
	var st standing
	if root.ProjectID.Valid {
		role, err := a.roles.ProjectRole(ctx, p, root.ProjectID)
		if err != nil {
			return st, err
		}
		st.role = role
	}
	m, ok := memberOf(p)
	if !ok {
		return st, nil
	}
	cm, err := a.store.GetConversationMember(ctx, db.GetConversationMemberParams{ConversationID: root.ID, MemberKind: m.Kind, MemberID: m.ID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return st, nil
	case err != nil:
		return st, err
	}
	st.member, st.owner = true, cm.Role == RoleOwner
	return st, nil
}

// scopeOf reads the rules' facts off a conversation and its root: the root decides who may
// read, and either being archived stops writing.
func scopeOf(root, conv db.Conversation) scope {
	return scope{
		kind:       root.Kind,
		private:    root.Visibility == VisibilityPrivate,
		hasProject: root.ProjectID.Valid,
		archived:   root.ArchivedAt.Valid || conv.ArchivedAt.Valid,
	}
}

// readableListed answers whether a listed conversation is readable, from what the list already
// knows: the caller's role in the project and whether they are in it. Only a conversation that
// hangs under a message needs its root looked up.
func (a *Access) readableListed(ctx context.Context, conv db.Conversation, p auth.Principal, role auth.Role, joined bool) (bool, error) {
	if conv.ParentMessageID.Valid {
		return a.CanRead(ctx, conv, p)
	}
	return decide(scopeOf(conv, conv), standing{role: role, member: joined}).Read, nil
}
